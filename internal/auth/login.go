package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/config"
	"github.com/glue2flow/g2f/internal/netguard"
)

// Server-side login (OIDC authorization code flow with PKCE) for pages that
// glue2flow renders itself, such as the forms of form triggers. It uses the
// editor's public client, so the page's URL must be one of the client's valid
// redirect URIs (e.g. https://g2f.example.com/*).

// Disabled reports whether authentication is turned off (G2F_AUTH_MODE=none).
func (a *Authenticator) Disabled() bool { return a.cfg.Mode == config.AuthNone }

// DevUser returns the static user used when authentication is disabled.
func (a *Authenticator) DevUser() *User {
	u, _ := a.Authenticate(&http.Request{Header: http.Header{}})
	return u
}

// PKCEChallenge returns the S256 code challenge of a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// LoginURL returns the Keycloak URL that signs the user in and redirects to
// redirectURI with ?code and ?state.
func (a *Authenticator) LoginURL(redirectURI, state, verifier string) string {
	q := url.Values{
		"client_id":             {a.cfg.ClientID},
		"response_type":         {"code"},
		"scope":                 {"openid"},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {PKCEChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	return a.cfg.Issuer + "/protocol/openid-connect/auth?" + q.Encode()
}

func (a *Authenticator) tokenURL() string {
	if a.cfg.TokenURL != "" {
		return a.cfg.TokenURL
	}
	if base, ok := strings.CutSuffix(a.cfg.JWKSURL, "/certs"); ok {
		return base + "/token"
	}
	return a.cfg.Issuer + "/protocol/openid-connect/token"
}

var loginClient = &http.Client{Timeout: 15 * time.Second, Transport: netguard.Unguarded()}

// Exchange redeems an authorization code and returns the signed-in user.
func (a *Authenticator) Exchange(ctx context.Context, code, verifier, redirectURI string) (*User, error) {
	if a.Disabled() {
		return a.DevUser(), nil
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {a.cfg.ClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := loginClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("token response (HTTP %d): %w", res.StatusCode, err)
	}
	if tok.AccessToken == "" {
		if tok.Error != "" {
			return nil, fmt.Errorf("token request: %s: %s", tok.Error, tok.Description)
		}
		return nil, errors.New("token response without access token")
	}
	return a.verify(ctx, tok.AccessToken)
}
