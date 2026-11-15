package api

import (
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The client-IP middleware is observable through the per-client rate limit of
// form access codes (20 codes per client address and hour). httptest
// requests come from 192.0.2.1.
func TestClientIPMiddlewareInstalled(t *testing.T) {
	requestCodes := func(t *testing.T, s *testServer, n int) (limited int) {
		t.Helper()
		s.as(orgAdmin).must(http.StatusOK, "PUT", "/api/plugins/email/config",
			map[string]any{"values": map[string]any{"smtpHost": "127.0.0.1"}}) // no sender: sending fails fast
		id := s.as(alice).createWF("protected", formGraph("email"), map[string]any{"active": true})["id"].(string)
		for i := range n {
			form := url.Values{"email": {fmt.Sprintf("user%d@example.com", i)}}
			rec := s.anon().raw("POST", "/form/"+id+"/form/code", strings.NewReader(form.Encode()), http.Header{
				"Content-Type":    {"application/x-www-form-urlencoded"},
				"X-Forwarded-For": {fmt.Sprintf("203.0.113.%d", i+1)},
			})
			switch rec.Code {
			case http.StatusTooManyRequests:
				limited++
			case http.StatusBadGateway: // counted, then the e-mail could not be sent
			default:
				t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
			}
		}
		return limited
	}
	t.Run("X-Forwarded-For from an untrusted peer is ignored", func(t *testing.T) {
		s := newTestServer(t)
		if got := requestCodes(t, s, 22); got != 2 {
			t.Fatalf("%d of 22 requests rate limited, want 2: forged X-Forwarded-For headers must not reset the limit", got)
		}
	})
	t.Run("X-Forwarded-For from a trusted proxy is used", func(t *testing.T) {
		s := newTestServerWith(t, serverOptions{trustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}})
		if got := requestCodes(t, s, 22); got != 0 {
			t.Fatalf("%d requests rate limited although each came from another client behind the proxy", got)
		}
	})
}

func TestRequestBodyLimits(t *testing.T) {
	s := newTestServer(t)
	a := s.as(orgAdmin)
	huge := strings.Repeat("x", 6<<20)
	for _, r := range []struct{ method, path, body string }{
		{"POST", "/api/workflows", `{"name":"` + huge + `"}`},
		{"POST", "/api/connections", `{"module":"github","name":"` + huge + `"}`},
		{"PUT", "/api/plugins/gmail/config", `{"values":{"clientId":"` + huge + `"}}`},
	} {
		rec := a.raw(r.method, r.path, strings.NewReader(r.body), http.Header{"Content-Type": {"application/json"}})
		if rec.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, decodeBody(t, rec)), "invalid JSON") {
			t.Errorf("%s %s with 6 MiB: %d %.200s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	if n := len(list(t, a.must(http.StatusOK, "GET", "/api/workflows", nil))); n != 0 {
		t.Fatalf("oversized workflow stored")
	}
	id := a.createWF("hook", hookGraph("<omit>", "none"), map[string]any{"active": true})["id"].(string)
	if rec := s.anon().raw("POST", "/webhook/"+id+"/hook", strings.NewReader(huge), nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized webhook body: %d", rec.Code)
	}
}

func TestAPIErrorsAreJSON(t *testing.T) {
	s := newTestServer(t)
	for _, c := range []*client{s.anon(), s.as(alice)} {
		for _, p := range []string{"/api/nope", "/api/workflows/x/nope", "/api/", "/api/index.html"} {
			rec := c.raw("GET", p, nil, nil)
			if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") ||
				errorOf(t, decodeBody(t, rec)) != "not found" {
				t.Errorf("GET %s: %d %v %s", p, rec.Code, rec.Header(), rec.Body)
			}
		}
	}
	// Handler errors use the same shape.
	code, body := s.as(alice).do("GET", "/api/workflows/00000000-0000-0000-0000-000000000000", nil)
	if code != http.StatusNotFound || errorOf(t, body) != "not found" {
		t.Fatalf("%d %v", code, body)
	}

	t.Run("unsupported method (current behaviour)", func(t *testing.T) {
		// chi answers 405 itself, before authentication and without the
		// JSON error body the handlers use. Pinned so a change is noticed.
		rec := s.anon().raw("PATCH", "/api/workflows", nil, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%d", rec.Code)
		}
		t.Logf("PATCH /api/workflows without token: %d, body %q, Allow %q", rec.Code, rec.Body, rec.Header().Get("Allow"))
	})
}

func TestSPAFileServer(t *testing.T) {
	root := t.TempDir()
	static := filepath.Join(root, "static")
	writeFile(t, static, "index.html", "<html>INDEX</html>")
	writeFile(t, static, "assets/app-123.js", "console.log('APP')")
	writeFile(t, static, "robots.txt", "ROBOTS")
	writeFile(t, root, "secret.txt", "TOP-SECRET")
	s := newTestServerWith(t, serverOptions{staticDir: static})
	c := s.anon()

	for path, want := range map[string]string{
		"/":                  "INDEX",
		"/workflows/123":     "INDEX", // client route
		"/robots.txt":        "ROBOTS",
		"/assets/app-123.js": "APP",
		"/assets/":           "INDEX", // directory: fall back
	} {
		rec := c.raw("GET", path, nil, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s: %d %q, want %q", path, rec.Code, rec.Body, want)
		}
	}
	if cc := c.raw("GET", "/assets/app-123.js", nil, nil).Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("assets not cached: %q", cc)
	}
	if cc := c.raw("GET", "/workflows/123", nil, nil).Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index cached: %q", cc)
	}

	t.Run("path traversal never leaves the static directory", func(t *testing.T) {
		for _, p := range []string{
			"/../secret.txt", "/%2e%2e/secret.txt", "/..%2fsecret.txt", "/%2e%2e%2fsecret.txt",
			"/assets/../../secret.txt", "/assets/%2e%2e/%2e%2e/secret.txt", "/assets/..%2f..%2fsecret.txt",
			"/..%5csecret.txt", "/static/../secret.txt", "/../../../../etc/passwd", "/%2e%2e/%2e%2e/%2e%2e/etc/passwd",
		} {
			rec := c.raw("GET", p, nil, nil)
			body := rec.Body.String()
			if strings.Contains(body, "TOP-SECRET") || strings.Contains(body, "root:") {
				t.Errorf("GET %s escaped the static directory: %d %q", p, rec.Code, body)
			}
			if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
				t.Errorf("GET %s: unexpected status %d", p, rec.Code)
			}
		}
	})

	t.Run("without a built editor", func(t *testing.T) {
		empty := newTestServer(t)
		rec := empty.anon().raw("GET", "/", nil, nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "editor not built") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	if _, err := os.Stat(filepath.Join(root, "secret.txt")); err != nil {
		t.Fatal(err)
	}
}
