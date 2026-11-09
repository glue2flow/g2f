package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/glue2flow/g2f/internal/config"
)

// fakeIdP serves a JWKS and a token endpoint that issues signed access tokens.
func fakeIdP(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL + "/realms/g2f"
	mux.HandleFunc("/realms/g2f/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/realms/g2f/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("code") != "good" || r.PostForm.Get("code_verifier") != "verifier" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Code not valid"}`))
			return
		}
		claims, _ := json.Marshal(map[string]any{
			"iss": issuer, "sub": "u1", "azp": "g2f", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(),
			"preferred_username": "ada", "name": "Ada Lovelace", "email": "ada@acme.test", "org": "acme",
		})
		jws, err := signer.Sign(claims)
		if err != nil {
			t.Error(err)
			return
		}
		tok, _ := jws.CompactSerialize()
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": tok, "token_type": "Bearer"})
	})
	return srv, &issuer
}

func TestLoginExchange(t *testing.T) {
	_, issuer := fakeIdP(t)
	a, err := New(context.Background(), config.AuthConfig{Mode: config.AuthOIDC, Issuer: *issuer, ClientID: "g2f", OrgClaim: "org", DefaultOrg: "default"})
	if err != nil {
		t.Fatal(err)
	}
	login, _ := url.Parse(a.LoginURL("https://g2f.test/form/w/n/callback", "st", "verifier"))
	q := login.Query()
	if !strings.HasPrefix(login.String(), *issuer+"/protocol/openid-connect/auth?") || q.Get("code_challenge") != PKCEChallenge("verifier") ||
		q.Get("code_challenge_method") != "S256" || q.Get("state") != "st" || q.Get("client_id") != "g2f" {
		t.Fatalf("login URL %s", login)
	}
	u, err := a.Exchange(context.Background(), "good", "verifier", "https://g2f.test/form/w/n/callback")
	if err != nil {
		t.Fatal(err)
	}
	if u.Sub != "u1" || u.Username != "ada" || u.Email != "ada@acme.test" || u.Org != "acme" {
		t.Fatalf("user %+v", u)
	}
	if _, err := a.Exchange(context.Background(), "bad", "verifier", "x"); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("bad code: %v", err)
	}
}

func TestTokenURL(t *testing.T) {
	cases := []struct {
		cfg  config.AuthConfig
		want string
	}{
		{config.AuthConfig{Issuer: "http://kc/realms/r"}, "http://kc/realms/r/protocol/openid-connect/token"},
		{config.AuthConfig{Issuer: "http://localhost:8081/realms/r", JWKSURL: "http://keycloak:8080/realms/r/protocol/openid-connect/certs"},
			"http://keycloak:8080/realms/r/protocol/openid-connect/token"},
		{config.AuthConfig{Issuer: "x", JWKSURL: "y/certs", TokenURL: "http://t/token"}, "http://t/token"},
	}
	for _, c := range cases {
		if got := (&Authenticator{cfg: c.cfg}).tokenURL(); got != c.want {
			t.Errorf("tokenURL = %q, want %q", got, c.want)
		}
	}
}
