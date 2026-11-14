// Package oauth runs the OAuth2 authorization code flow for plugins and keeps
// the resulting access tokens fresh.
//
// Flow: the editor asks for an authorize URL (POST /api/connections/authorize),
// the user consents at the provider, the provider redirects to
// {publicUrl}/oauth/callback, and the code is exchanged for tokens which are
// stored encrypted as a connection. The OAuth state is stateless: it is an
// encrypted, expiring blob bound to the user, module and PKCE verifier, and to
// the initiating browser through a nonce cookie (see CookieName).
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/auth"
	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/netguard"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/secrets"
	"github.com/glue2flow/g2f/internal/store"
)

const (
	stateTTL      = 10 * time.Minute
	refreshMargin = 60 * time.Second
	stateAAD      = "oauth-state"
)

// CookieName is the cookie that binds a flow to the browser that started it.
// Its value is the nonce returned by AuthorizeURL; the state only holds a hash.
const CookieName = "g2f_oauth"

// StateTTL is how long a started flow stays valid (also the cookie lifetime).
const StateTTL = stateTTL

var ErrNotFound = errors.New("connection not found")

// Store is the persistence the manager needs.
type Store interface {
	CreateConnection(ctx context.Context, c *store.Connection) error
	GetConnection(ctx context.Context, id string) (*store.Connection, error)
	RefreshConnection(ctx context.Context, id string, fn func(c *store.Connection) (bool, error)) (*store.Connection, error)
}

// Settings provides plugin config (where the OAuth app credentials live).
type Settings interface {
	ModuleSettings(ctx context.Context, org, module string) (config, vars map[string]any, err error)
}

type Manager struct {
	store     Store
	box       *secrets.Box
	reg       *nodes.Registry
	settings  Settings
	publicURL string
	client    *http.Client
	now       func() time.Time
}

func NewManager(st Store, box *secrets.Box, reg *nodes.Registry, settings Settings, publicURL string) *Manager {
	return &Manager{
		store: st, box: box, reg: reg, settings: settings,
		publicURL: strings.TrimRight(publicURL, "/"),
		client:    &http.Client{Timeout: 30 * time.Second, Transport: netguard.Unguarded()},
		now:       time.Now,
	}
}

// RedirectURL must be registered as redirect/callback URL at the provider.
func (m *Manager) RedirectURL() string { return m.publicURL + "/oauth/callback" }

type flowState struct {
	Sub      string    `json:"sub"`
	Owner    string    `json:"owner"`
	Org      string    `json:"org"`
	Module   string    `json:"module"`
	Name     string    `json:"name"`
	Verifier string    `json:"verifier,omitempty"`
	Nonce    string    `json:"nonce"` // base64url sha256 of the browser cookie value
	Expires  time.Time `json:"exp"`
}

func (m *Manager) spec(module string) (*nodes.ModuleInfo, *nodes.OAuth2Spec, error) {
	info, ok := m.reg.Module(module)
	if !ok || info.OAuth2 == nil {
		return nil, nil, fmt.Errorf("plugin %q does not support connected accounts", module)
	}
	return info, info.OAuth2, nil
}

type client struct{ id, secret string }

func (m *Manager) appCredentials(ctx context.Context, org, module string, spec *nodes.OAuth2Spec) (client, error) {
	config, _, err := m.settings.ModuleSettings(ctx, org, module)
	if err != nil {
		return client{}, err
	}
	c := client{id: expression.Stringify(config[spec.ClientIDConfig]), secret: expression.Stringify(config[spec.ClientSecretConfig])}
	if c.id == "" || c.secret == "" {
		return client{}, fmt.Errorf("an organization admin must first set %s and %s in the plugin settings", spec.ClientIDConfig, spec.ClientSecretConfig)
	}
	return c, nil
}

func nonceHash(nonce string) string {
	sum := sha256.Sum256([]byte(nonce))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL starts the flow for u and returns the provider URL to open and
// a nonce. The caller must hand the nonce to the initiating browser as the
// CookieName cookie and pass it back to Callback.
func (m *Manager) AuthorizeURL(ctx context.Context, u *auth.User, module, name string) (authURL, nonce string, err error) {
	info, spec, err := m.spec(module)
	if err != nil {
		return "", "", err
	}
	app, err := m.appCredentials(ctx, u.Org, module, spec)
	if err != nil {
		return "", "", err
	}
	if name = strings.TrimSpace(name); name == "" {
		name = info.Name + " account"
	}
	nonce = randomString(32)
	st := flowState{Sub: u.Sub, Owner: u.Name, Org: u.Org, Module: module, Name: name, Nonce: nonceHash(nonce), Expires: m.now().Add(stateTTL)}
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {app.id},
		"redirect_uri":  {m.RedirectURL()},
	}
	if len(spec.Scopes) > 0 {
		q.Set("scope", strings.Join(spec.Scopes, spec.ScopeSeparator))
	}
	if spec.PKCE {
		st.Verifier = randomString(48)
		sum := sha256.Sum256([]byte(st.Verifier))
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
		q.Set("code_challenge_method", "S256")
	}
	for k, v := range spec.AuthorizeParams {
		q.Set(k, v)
	}
	raw, _ := json.Marshal(st)
	sealed, err := m.box.Seal(string(raw), stateAAD)
	if err != nil {
		return "", "", err
	}
	q.Set("state", sealed)
	sep := "?"
	if strings.Contains(spec.AuthorizeURL, "?") {
		sep = "&"
	}
	return spec.AuthorizeURL + sep + q.Encode(), nonce, nil
}

// Callback completes the flow and stores the new connection. nonce is the
// value of the CookieName cookie the browser sent; it must match the flow.
func (m *Manager) Callback(ctx context.Context, code, state, nonce string) (*store.Connection, error) {
	raw, err := m.box.Open(state, stateAAD)
	if err != nil {
		return nil, errors.New("invalid or tampered state")
	}
	var st flowState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, errors.New("invalid state")
	}
	if m.now().After(st.Expires) {
		return nil, errors.New("the sign-in took too long, please try again")
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(nonceHash(nonce)), []byte(st.Nonce)) != 1 {
		return nil, errors.New("this sign-in was started in another browser, please start again from the Connections page")
	}
	_, spec, err := m.spec(st.Module)
	if err != nil {
		return nil, err
	}
	app, err := m.appCredentials(ctx, st.Org, st.Module, spec)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {m.RedirectURL()},
	}
	if st.Verifier != "" {
		form.Set("code_verifier", st.Verifier)
	}
	tok, err := m.tokenRequest(ctx, spec, app, form)
	if err != nil {
		return nil, err
	}

	c := &store.Connection{
		OrgID: st.Org, Module: st.Module, Kind: store.KindOAuth2, Name: st.Name, OwnerSub: st.Sub, OwnerName: st.Owner,
		Scope: "private", Groups: []string{},
	}
	c.ID = newID()
	if spec.InstanceURLField != "" && tok.InstanceURL == "" {
		return nil, fmt.Errorf("provider returned no %s", spec.InstanceURLField)
	}
	if err := m.applyToken(c, tok); err != nil {
		return nil, err
	}
	if spec.UserInfo != nil {
		c.Account = m.accountName(ctx, spec, tok)
	}
	if err := m.store.CreateConnection(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Token returns a valid access token of a connection, refreshing it if it
// expires soon. org and module must match the connection.
func (m *Manager) Token(ctx context.Context, org, module, id string) (*nodes.OAuthToken, error) {
	c, err := m.store.GetConnection(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (c.OrgID != org || c.Module != module || c.Kind == store.KindCredential)) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if m.expiring(c) {
		name := c.Name
		_, spec, err := m.spec(module)
		if err != nil {
			return nil, err
		}
		c, err = m.store.RefreshConnection(ctx, id, func(c *store.Connection) (bool, error) {
			if !m.expiring(c) {
				return false, nil // refreshed meanwhile by someone else
			}
			return true, m.refresh(ctx, spec, c)
		})
		if err != nil {
			return nil, fmt.Errorf("refresh %s token of %q: %w", module, name, err)
		}
	}
	access, err := m.box.Open(c.AccessToken, aad(c.ID, "access"))
	if err != nil {
		return nil, err
	}
	return &nodes.OAuthToken{AccessToken: access, TokenType: c.TokenType, Account: c.Account, InstanceURL: c.InstanceURL}, nil
}

func (m *Manager) expiring(c *store.Connection) bool {
	return c.ExpiresAt != nil && m.now().Add(refreshMargin).After(*c.ExpiresAt)
}

func (m *Manager) refresh(ctx context.Context, spec *nodes.OAuth2Spec, c *store.Connection) error {
	if c.RefreshToken == "" {
		return errors.New("the access token expired and the provider gave no refresh token; reconnect the account")
	}
	refresh, err := m.box.Open(c.RefreshToken, aad(c.ID, "refresh"))
	if err != nil {
		return err
	}
	app, err := m.appCredentials(ctx, c.OrgID, c.Module, spec)
	if err != nil {
		return err
	}
	tok, err := m.tokenRequest(ctx, spec, app, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if err != nil {
		return err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refresh // most providers keep the refresh token
	}
	return m.applyToken(c, tok)
}

type tokenResponse struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Scope        string
	ExpiresIn    int64
	InstanceURL  string // validated; set if the spec names instanceUrlField and the response has it
}

func (m *Manager) tokenRequest(ctx context.Context, spec *nodes.OAuth2Spec, app client, form url.Values) (*tokenResponse, error) {
	if spec.TokenAuth == "basic" {
		form.Del("client_id")
	} else {
		form.Set("client_id", app.id)
		form.Set("client_secret", app.secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if spec.TokenAuth == "basic" {
		req.SetBasicAuth(url.QueryEscape(app.id), url.QueryEscape(app.secret))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	fields := map[string]any{}
	if err := json.Unmarshal(body, &fields); err != nil {
		// Some providers answer form-encoded.
		vals, perr := url.ParseQuery(string(body))
		if perr != nil {
			return nil, fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
		}
		for k := range vals {
			fields[k] = vals.Get(k)
		}
	}
	if e := expression.Stringify(fields["error"]); e != "" || resp.StatusCode >= 400 {
		desc := expression.Stringify(fields["error_description"])
		if e == "" {
			e = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return nil, fmt.Errorf("provider refused the token request: %s %s", e, desc)
	}
	t := &tokenResponse{
		AccessToken:  expression.Stringify(fields["access_token"]),
		RefreshToken: expression.Stringify(fields["refresh_token"]),
		TokenType:    expression.Stringify(fields["token_type"]),
		Scope:        expression.Stringify(fields["scope"]),
	}
	switch v := fields["expires_in"].(type) {
	case float64:
		t.ExpiresIn = int64(v)
	case string:
		t.ExpiresIn, _ = strconv.ParseInt(v, 10, 64)
	}
	if t.AccessToken == "" {
		return nil, errors.New("provider returned no access token")
	}
	if spec.InstanceURLField != "" {
		if raw := expression.Stringify(fields[spec.InstanceURLField]); raw != "" {
			if t.InstanceURL, err = checkInstanceURL(spec, raw); err != nil {
				return nil, err
			}
		}
	}
	return t, nil
}

// checkInstanceURL accepts an https origin (no userinfo, path, query or
// fragment) whose host the module may send the token to, and returns it
// without a trailing slash.
func checkInstanceURL(spec *nodes.OAuth2Spec, raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Scheme != "https" || u.Hostname() == "":
		return "", fmt.Errorf("provider returned an invalid %s %q: it must be an https URL with a host", spec.InstanceURLField, raw)
	case u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "?#"):
		return "", fmt.Errorf("provider returned an invalid %s %q: it must not have credentials, a path, a query or a fragment", spec.InstanceURLField, raw)
	case !spec.AllowsURL(u):
		return "", fmt.Errorf("provider returned %s %q, whose host is not in the plugin's oauth2.apiHosts", spec.InstanceURLField, raw)
	}
	return "https://" + u.Host, nil
}

func (m *Manager) applyToken(c *store.Connection, t *tokenResponse) error {
	var err error
	if c.AccessToken, err = m.box.Seal(t.AccessToken, aad(c.ID, "access")); err != nil {
		return err
	}
	c.RefreshToken = ""
	if t.RefreshToken != "" {
		if c.RefreshToken, err = m.box.Seal(t.RefreshToken, aad(c.ID, "refresh")); err != nil {
			return err
		}
	}
	if t.InstanceURL != "" {
		c.InstanceURL = t.InstanceURL // a refresh response may omit it: keep the stored one
	}
	c.TokenType = t.TokenType
	if c.TokenType == "" {
		c.TokenType = "Bearer"
	}
	if t.Scope != "" {
		c.Scopes = t.Scope
	}
	c.ExpiresAt = nil
	if t.ExpiresIn > 0 {
		exp := m.now().Add(time.Duration(t.ExpiresIn) * time.Second).UTC()
		c.ExpiresAt = &exp
	}
	return nil
}

// accountName reads a display name (e.g. the email) from the userinfo endpoint.
// Failures are ignored: the label is cosmetic.
func (m *Manager) accountName(ctx context.Context, spec *nodes.OAuth2Spec, t *tokenResponse) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.UserInfo.URL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", (&nodes.OAuthToken{AccessToken: t.AccessToken, TokenType: t.TokenType}).AuthorizationHeader())
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var info map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info) != nil {
		return ""
	}
	return expression.Stringify(info[spec.UserInfo.Field])
}

func aad(id, kind string) string { return "conn/" + id + "/" + kind }

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
