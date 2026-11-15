package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// publicRoutes are the routes reachable without a token. Anything else that
// is registered must answer 401 without one. Keep this list in sync with
// server.go on purpose: a new public route needs a deliberate decision.
var publicRoutes = map[string]string{
	"/healthz":          "load balancer health check",
	"/api/config":       "the SPA needs the Keycloak settings before it can sign in",
	"/webhook/*":        "webhook triggers (own token/HMAC auth)",
	"/form/*":           "form triggers (own protection)",
	"/webhook-resume/*": "signed resume links of Wait nodes",
	"/oauth/callback":   "OAuth2 provider redirect (state + cookie bound)",
	"/push/*":           "mail push notifications (secret path / clientState)",
}

func TestEveryRouteOutsideAllowListRequiresToken(t *testing.T) {
	// Catalogs enabled so that the conditional catalog routes are registered too.
	s := newTestServerWith(t, serverOptions{catalogs: []string{"https://catalog.example.com/index.json"}})
	routes, ok := s.handler.(chi.Routes)
	if !ok {
		t.Fatalf("handler is %T, not a chi router", s.handler)
	}
	seenPublic := map[string]bool{}
	checked := 0
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if _, public := publicRoutes[route]; public {
			seenPublic[route] = true
			return nil
		}
		path := strings.NewReplacer("{id}", "00000000-0000-0000-0000-000000000001", "{module}", "github", "*", "x").Replace(route)
		t.Run(method+" "+route, func(t *testing.T) {
			rec := s.anon().raw(method, path, strings.NewReader("{}"), http.Header{"Content-Type": {"application/json"}})
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s without token: status %d, want 401 (route outside the auth group?)", method, path, rec.Code)
			}
			if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("no WWW-Authenticate challenge: %v", rec.Header())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type %q, want application/json", ct)
			}
		})
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 20 {
		t.Fatalf("only %d protected routes found; did the walk work?", checked)
	}
	for route := range publicRoutes {
		if !seenPublic[route] {
			t.Errorf("allow-listed public route %s is not registered any more; update publicRoutes", route)
		}
	}
}

func TestPublicEndpointsWithoutToken(t *testing.T) {
	s := newTestServer(t)
	c := s.anon()

	rec := c.raw("GET", "/healthz", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body)
	}

	cfg := obj(t, c.must(http.StatusOK, "GET", "/api/config", nil))
	if cfg["authMode"] != "oidc" || cfg["clientId"] != testClientID || cfg["realm"] != "g2f" ||
		cfg["keycloakUrl"] != idp.srv.URL || cfg["oauthRedirectUrl"] != testPublicURL+"/oauth/callback" {
		t.Fatalf("config: %v", cfg)
	}
	if rec := c.raw("GET", "/api/config", nil, nil); !strings.Contains(rec.Header().Get("Cache-Control"), "no-cache") {
		t.Errorf("API responses must not be cached: %v", rec.Header())
	}
}

func TestAuthentication(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	mutate := func(f func(c map[string]any)) string {
		c := alice.claims()
		f(c)
		return idp.sign(t, idp.signer, c)
	}
	unsignedPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + idp.issuer + `","sub":"alice","azp":"g2f","org":"acme","exp":` +
		strconv.FormatInt(now.Add(time.Hour).Unix(), 10) + `}`))

	rejected := []struct {
		name   string
		header string
	}{
		{"no Authorization header", ""},
		{"basic auth", "Basic YWxpY2U6c2VjcmV0"},
		{"empty bearer", "Bearer "},
		{"malformed token", "Bearer not.a.jwt"},
		{"alg none", "Bearer " + base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + unsignedPayload + "."},
		{"expired", "Bearer " + mutate(func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() })},
		{"wrong issuer", "Bearer " + mutate(func(c map[string]any) { c["iss"] = "https://evil.example.com/realms/g2f" })},
		{"other client (azp and aud)", "Bearer " + mutate(func(c map[string]any) { c["azp"] = "other-app" })},
		{"signed by an unknown key with a known kid", "Bearer " + idp.sign(t, idp.rogue, alice.claims())},
		{"tampered payload", "Bearer " + tamper(alice.token(t))},
	}
	for _, c := range rejected {
		t.Run("rejects "+c.name, func(t *testing.T) {
			hdr := http.Header{}
			if c.header != "" {
				hdr.Set("Authorization", c.header)
			}
			rec := s.anon().raw("GET", "/api/me", nil, hdr)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if rec.Header().Get("WWW-Authenticate") != `Bearer realm="g2f"` {
				t.Errorf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
			}
			if rec.Header().Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type %q", rec.Header().Get("Content-Type"))
			}
			if got := errorOf(t, decodeBody(t, rec)); got != "unauthorized" {
				t.Errorf("error %q", got)
			}
		})
	}

	t.Run("accepts a valid token and maps the claims", func(t *testing.T) {
		me := obj(t, s.as(alice).must(http.StatusOK, "GET", "/api/me", nil))
		if me["sub"] != "alice" || me["org"] != "acme" || me["admin"] != false || me["pluginAdmin"] != false ||
			mustJSON(t, me["groups"]) != `["/eng/backend"]` {
			t.Fatalf("me: %v", me)
		}
	})
	t.Run("accepts a token for the client in aud instead of azp", func(t *testing.T) {
		tok := mutate(func(c map[string]any) { c["azp"] = "other-app"; c["aud"] = []any{"account", testClientID} })
		s.withToken(tok).must(http.StatusOK, "GET", "/api/me", nil)
	})
	t.Run("realm roles grant admin and plugin admin", func(t *testing.T) {
		me := obj(t, s.as(orgAdmin).must(http.StatusOK, "GET", "/api/me", nil))
		if me["admin"] != true || me["pluginAdmin"] != false {
			t.Fatalf("org admin: %v", me)
		}
		me = obj(t, s.as(pluginAdmin).must(http.StatusOK, "GET", "/api/me", nil))
		if me["admin"] != false || me["pluginAdmin"] != true {
			t.Fatalf("plugin admin: %v", me)
		}
	})
	t.Run("token without org claim falls back to the default org", func(t *testing.T) {
		tok := mutate(func(c map[string]any) { delete(c, "org") })
		me := obj(t, s.withToken(tok).must(http.StatusOK, "GET", "/api/me", nil))
		if me["org"] != "default" {
			t.Fatalf("org %v", me["org"])
		}
	})
}

// tamper changes the payload of a JWS without re-signing it.
func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	payload = []byte(strings.Replace(string(payload), `"org":"acme"`, `"org":"evil"`, 1))
	parts[1] = base64.RawURLEncoding.EncodeToString(payload)
	return strings.Join(parts, ".")
}
