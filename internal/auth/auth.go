package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/glue2flow/g2f/internal/config"
)

// Authenticator turns bearer tokens into Users.
type Authenticator struct {
	cfg      config.AuthConfig
	verifier *oidc.IDTokenVerifier
}

func New(ctx context.Context, cfg config.AuthConfig) (*Authenticator, error) {
	a := &Authenticator{cfg: cfg}
	if cfg.Mode == config.AuthNone {
		return a, nil
	}

	jwks := cfg.JWKSURL
	if jwks == "" {
		// Keycloak's well-known location; avoids a discovery round trip at startup
		// so glue2flow can boot before Keycloak is reachable.
		jwks = cfg.Issuer + "/protocol/openid-connect/certs"
	}
	keySet := oidc.NewRemoteKeySet(ctx, jwks)
	a.verifier = oidc.NewVerifier(cfg.Issuer, keySet, &oidc.Config{
		// Keycloak access tokens carry aud=account by default; we check azp instead.
		SkipClientIDCheck: true,
	})
	return a, nil
}

// Middleware rejects requests without a valid token and stores the User in the context.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.Authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="g2f"`)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

func (a *Authenticator) Authenticate(r *http.Request) (*User, error) {
	if a.cfg.Mode == config.AuthNone {
		return &User{
			Sub:         "dev-" + a.cfg.DevUser,
			Username:    a.cfg.DevUser,
			Name:        a.cfg.DevUser,
			Org:         a.cfg.DefaultOrg,
			Groups:      a.cfg.DevGroups,
			Roles:       []string{a.cfg.AdminRole, a.cfg.PluginAdminRole},
			Admin:       true,
			PluginAdmin: true,
		}, nil
	}

	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return nil, fmt.Errorf("missing bearer token")
	}
	return a.verify(r.Context(), raw)
}

func (a *Authenticator) verify(ctx context.Context, raw string) (*User, error) {
	tok, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}

	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, err
	}
	if azp, _ := claims["azp"].(string); a.cfg.ClientID != "" && azp != a.cfg.ClientID && !slices.Contains(tok.Audience, a.cfg.ClientID) {
		return nil, fmt.Errorf("token not issued for client %q", a.cfg.ClientID)
	}
	return a.userFromClaims(tok.Subject, claims), nil
}

func (a *Authenticator) userFromClaims(sub string, c map[string]any) *User {
	u := &User{
		Sub:      sub,
		Username: str(c["preferred_username"]),
		Name:     str(c["name"]),
		Email:    str(c["email"]),
		Groups:   strList(c[a.cfg.GroupsClaim]),
		Org:      orgFromClaim(c[a.cfg.OrgClaim]),
	}
	if u.Name == "" {
		u.Name = u.Username
	}
	if u.Org == "" {
		u.Org = a.cfg.DefaultOrg
	}
	if ra, ok := c["realm_access"].(map[string]any); ok {
		u.Roles = strList(ra["roles"])
	}
	u.Admin = slices.Contains(u.Roles, a.cfg.AdminRole)
	u.PluginAdmin = slices.Contains(u.Roles, a.cfg.PluginAdminRole)
	return u
}

// orgFromClaim supports a plain string attribute ("acme"), a list (["acme"]) and
// the map form emitted by Keycloak Organizations ({"acme": {...}}).
func orgFromClaim(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		if l := strList(t); len(l) > 0 {
			return l[0]
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			return keys[0]
		}
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	case json.RawMessage:
		var out []string
		_ = json.Unmarshal(t, &out)
		return out
	}
	return nil
}
