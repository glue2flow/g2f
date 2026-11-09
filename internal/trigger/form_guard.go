package trigger

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/auth"
)

// Form protection: signed tokens and cookies, captcha questions and access
// codes. Tokens are stateless (HMAC with a key derived from the server secret),
// so every replica can check them; one-time use and attempt limits are kept in
// the workflow state store, which all replicas share.

const (
	captchaTTL     = time.Hour
	codeTTL        = 15 * time.Minute
	loginStateTTL  = 10 * time.Minute
	formSessionTTL = 12 * time.Hour

	codeTriesPerCode   = 5
	codesPerEmailHour  = 5
	codesPerClientHour = 20
)

// FormState is the shared state used for one-time tokens and rate limits
// (implemented by *store.Store).
type FormState interface {
	StateIncrement(ctx context.Context, workflowID, scope, key string, by float64, ttl time.Duration) (float64, error)
	StateRemember(ctx context.Context, workflowID, scope string, keys []string, ttl time.Duration) ([]bool, error)
}

// FormAuth signs users in with the organization's identity provider
// (implemented by *auth.Authenticator).
type FormAuth interface {
	Disabled() bool
	DevUser() *auth.User
	LoginURL(redirectURI, state, verifier string) string
	Exchange(ctx context.Context, code, verifier, redirectURI string) (*auth.User, error)
}

// FormMailer sends access codes with the organization's e-mail settings.
type FormMailer interface {
	Configured(ctx context.Context, org string) (bool, error)
	SendText(ctx context.Context, org, to, subject, text string) error
}

// submitter identifies who answered a protected form.
type submitter struct {
	Sub      string `json:"sub,omitempty"`
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	Org      string `json:"org,omitempty"`
}

func (s *submitter) item() map[string]any {
	m := map[string]any{"email": s.Email}
	if s.Sub != "" {
		m["sub"], m["username"], m["name"] = s.Sub, s.Username, s.Name
	}
	return m
}

func (s *submitter) label() string {
	switch {
	case s.Name != "" && s.Email != "":
		return s.Name + " (" + s.Email + ")"
	case s.Name != "":
		return s.Name
	case s.Username != "":
		return s.Username
	}
	return s.Email
}

// Token kinds.
const (
	tokCaptcha = "captcha"
	tokCode    = "code"
	tokSession = "session"
	tokLogin   = "login"
)

type formClaims struct {
	Kind  string `json:"k"`
	WF    string `json:"w"`
	Node  string `json:"n"`
	Exp   int64  `json:"x"`
	Nonce string `json:"o,omitempty"`
	// Question is the captcha question shown to the user.
	Question string     `json:"q,omitempty"`
	Email    string     `json:"e,omitempty"`
	User     *submitter `json:"u,omitempty"`
	// Login flow: OAuth state, PKCE verifier and the query to return to.
	State    string `json:"s,omitempty"`
	Verifier string `json:"v,omitempty"`
	Return   string `json:"r,omitempty"`
}

var (
	errTokenInvalid = errors.New("invalid token")
	errTokenExpired = errors.New("token expired")
)

type formSealer struct{ key []byte }

func newFormSealer(secretKey string) formSealer {
	sum := sha256.Sum256([]byte("g2f-form\x00" + secretKey))
	return formSealer{key: sum[:]}
}

func (s formSealer) mac(payload []byte, secret string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write(payload)
	m.Write([]byte{0})
	m.Write([]byte(secret))
	return m.Sum(nil)
}

// seal signs claims. secret (a captcha answer, an access code) is part of
// the signature but not of the token, so only someone who knows it can open it.
func (s formSealer) seal(c formClaims, ttl time.Duration, secret string) string {
	c.Exp = time.Now().Add(ttl).Unix()
	payload, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(s.mac(payload, secret))
}

// peek decodes a token without checking it (e.g. to count attempts per nonce).
func peek(tok string) (*formClaims, []byte, []byte, error) {
	p, m, ok := strings.Cut(tok, ".")
	if !ok {
		return nil, nil, nil, errTokenInvalid
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(m)
	if err1 != nil || err2 != nil {
		return nil, nil, nil, errTokenInvalid
	}
	var c formClaims
	if json.Unmarshal(payload, &c) != nil {
		return nil, nil, nil, errTokenInvalid
	}
	return &c, payload, mac, nil
}

func (s formSealer) open(tok, kind, wf, node, secret string) (*formClaims, error) {
	c, payload, mac, err := peek(tok)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(mac, s.mac(payload, secret)) || c.Kind != kind || c.WF != wf || c.Node != node {
		return nil, errTokenInvalid
	}
	if time.Now().Unix() > c.Exp {
		return c, errTokenExpired
	}
	return c, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomInt(lo, hi int) int {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(hi-lo+1)))
	return lo + int(n.Int64())
}

// newCaptcha returns a question like "12 + 7" and its signed token.
func (s formSealer) newCaptcha(wf, node string) (question, token string) {
	a, b := randomInt(2, 19), randomInt(2, 19)
	var answer int
	if randomInt(0, 1) == 0 || a == b {
		question, answer = fmt.Sprintf("%d + %d", a, b), a+b
	} else {
		a, b = max(a, b), min(a, b)
		question, answer = fmt.Sprintf("%d − %d", a, b), a-b
	}
	c := formClaims{Kind: tokCaptcha, WF: wf, Node: node, Nonce: randomHex(12), Question: question}
	return question, s.seal(c, captchaTTL, strconv.Itoa(answer))
}

// newCode returns a six digit access code for email and its signed token.
func (s formSealer) newCode(wf, node, email string) (code, token string) {
	code = fmt.Sprintf("%06d", randomInt(0, 999999))
	c := formClaims{Kind: tokCode, WF: wf, Node: node, Nonce: randomHex(12), Email: email}
	return code, s.seal(c, codeTTL, code)
}
