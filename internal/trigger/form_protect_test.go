package trigger

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glue2flow/g2f/internal/auth"
	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/workflow"
)

// fakeState implements FormState in memory (TTLs ignored).
type fakeState struct {
	mu sync.Mutex
	n  map[string]float64
}

func newFakeState() *fakeState { return &fakeState{n: map[string]float64{}} }

func (s *fakeState) StateIncrement(_ context.Context, wf, scope, key string, by float64, _ time.Duration) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := wf + "|" + scope + "|" + key
	s.n[k] += by
	return s.n[k], nil
}

func (s *fakeState) StateRemember(_ context.Context, wf, scope string, keys []string, _ time.Duration) ([]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]bool, len(keys))
	for i, key := range keys {
		k := wf + "|" + scope + "|" + key
		out[i] = s.n[k] == 0
		s.n[k] = 1
	}
	return out, nil
}

type fakeMailer struct {
	configured bool
	mu         sync.Mutex
	sent       []string // "to|text"
}

func (m *fakeMailer) Configured(context.Context, string) (bool, error) { return m.configured, nil }

func (m *fakeMailer) SendText(_ context.Context, _, to, _, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, to+"|"+text)
	return nil
}

func (m *fakeMailer) lastCode(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("no e-mail sent")
	}
	code := regexp.MustCompile(`\b\d{6}\b`).FindString(m.sent[len(m.sent)-1])
	if code == "" {
		t.Fatal("no code in e-mail")
	}
	return code
}

type fakeAuth struct {
	disabled bool
	user     auth.User
}

func (a *fakeAuth) Disabled() bool { return a.disabled }
func (a *fakeAuth) DevUser() *auth.User {
	return &auth.User{Sub: "dev", Username: "dev", Name: "dev", Org: "acme"}
}
func (a *fakeAuth) LoginURL(redirect, state, verifier string) string {
	return "https://idp.example/auth?" + url.Values{"redirect_uri": {redirect}, "state": {state}, "challenge": {auth.PKCEChallenge(verifier)}}.Encode()
}
func (a *fakeAuth) Exchange(_ context.Context, code, verifier, redirect string) (*auth.User, error) {
	if code != "good" {
		return nil, fmt.Errorf("bad code")
	}
	u := a.user
	return &u, nil
}

type formEnv struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
	eng    *recordingEngine
	mailer *fakeMailer
	auth   *fakeAuth
	base   string
}

func newFormEnv(t *testing.T, params map[string]any) *formEnv {
	t.Helper()
	wf := formWF(true)
	wf.OrgID = "acme"
	for k, v := range params {
		wf.Graph.Nodes[0].Params[k] = v
	}
	env := &formEnv{t: t, eng: &recordingEngine{ch: make(chan engine.Request, 8)},
		mailer: &fakeMailer{configured: true},
		auth:   &fakeAuth{user: auth.User{Sub: "u1", Username: "ada", Name: "Ada", Email: "ada@acme.test", Org: "acme"}}}
	mux := http.NewServeMux()
	// X-Test-Peer lets a test pretend to connect from another address.
	env.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get("X-Test-Peer"); p != "" {
			r.RemoteAddr = p
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(env.srv.Close)
	h := NewFormHandler(newFakeStore(wf), env.eng, registry(t), discard, FormOptions{
		SecretKey: "k", PublicURL: env.srv.URL, State: newFakeState(), Auth: env.auth, Mailer: env.mailer,
	})
	mux.Handle("/form/", h)
	jar, _ := cookiejar.New(nil)
	env.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	env.base = env.srv.URL + "/form/wf/form"
	return env
}

func (e *formEnv) do(method, path string, form url.Values) (int, string, *http.Response) {
	e.t.Helper()
	u := path
	if !strings.HasPrefix(u, "http") {
		u = e.base + path
	}
	var res *http.Response
	var err error
	if method == "GET" {
		res, err = e.client.Get(u)
	} else {
		res, err = e.client.PostForm(u, form)
	}
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res
}

var answers = url.Values{"name": {"Ada"}, "team": {"Sales"}}

func with(v url.Values, kv ...string) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = vs
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out.Set(kv[i], kv[i+1])
	}
	return out
}

var (
	captchaRe = regexp.MustCompile(`(\d+) ([+−]) (\d+) = \?`)
	tokenRe   = regexp.MustCompile(`name="(?:_captcha|token)" value="([^"]+)"`)
)

func solve(t *testing.T, page string) (answer, token string) {
	t.Helper()
	m, tok := captchaRe.FindStringSubmatch(html.UnescapeString(page)), tokenRe.FindStringSubmatch(page)
	if m == nil || tok == nil {
		t.Fatalf("no captcha on page: %s", page)
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[3])
	if m[2] == "+" {
		return strconv.Itoa(a + b), tok[1]
	}
	return strconv.Itoa(a - b), tok[1]
}

func TestFormCaptcha(t *testing.T) {
	e := newFormEnv(t, map[string]any{"protection": "captcha"})
	_, page, _ := e.do("GET", "", nil)
	ans, tok := solve(t, page)

	// Missing answers keep the question and do not use it up.
	code, page, _ := e.do("POST", "", with(answers, "name", "", "_captcha", tok, "_answer", ans))
	if code != 422 || !strings.Contains(page, tok) {
		t.Fatalf("status %d, question not kept", code)
	}

	wrong, _ := strconv.Atoi(ans)
	code, page, _ = e.do("POST", "", with(answers, "_captcha", tok, "_answer", strconv.Itoa(wrong+1)))
	if code != 422 || !strings.Contains(page, "not right") || len(e.eng.reqs) != 0 {
		t.Fatalf("wrong answer accepted: %d", code)
	}
	// The question was used up by the wrong answer.
	code, _, _ = e.do("POST", "", with(answers, "_captcha", tok, "_answer", ans))
	if code != 422 {
		t.Fatalf("used question accepted: %d", code)
	}

	ans, tok = solve(t, page)
	code, _, _ = e.do("POST", "", with(answers, "_captcha", tok, "_answer", " 0"+ans+" "))
	if code != 303 || len(e.eng.reqs) != 1 {
		t.Fatalf("right answer rejected: %d", code)
	}
	if _, ok := e.eng.reqs[0].Items[0][nodes.FormSubmittedBy]; ok {
		t.Fatal("captcha forms have no submittedBy")
	}
	// Replaying the solved question fails.
	if code, _, _ = e.do("POST", "", with(answers, "_captcha", tok, "_answer", ans)); code != 422 {
		t.Fatalf("replay accepted: %d", code)
	}
}

func TestFormAccessCode(t *testing.T) {
	e := newFormEnv(t, map[string]any{"protection": "email", "emailDomains": "acme.test"})

	_, page, _ := e.do("GET", "?name=Ada", nil)
	if !strings.Contains(page, `action="/form/wf/form/code"`) || strings.Contains(page, `name="team"`) {
		t.Fatal("expected the e-mail step, not the form")
	}
	// Submitting without a confirmed address is refused.
	if code, _, _ := e.do("POST", "", answers); code != 403 || len(e.eng.reqs) != 0 {
		t.Fatalf("unconfirmed submission: %d", code)
	}
	if code, page, _ := e.do("POST", "/code", url.Values{"email": {"eve@evil.test"}}); code != 422 || !strings.Contains(page, "acme.test") {
		t.Fatalf("foreign domain: %d", code)
	}
	if code, _, _ := e.do("POST", "/code", url.Values{"email": {"not an address"}}); code != 422 {
		t.Fatalf("invalid address: %d", code)
	}

	code, page, _ := e.do("POST", "/code", url.Values{"email": {"Ada@Sub.ACME.test"}, "return": {"name=Ada"}})
	if code != 200 {
		t.Fatalf("request code: %d %s", code, page)
	}
	sent := e.mailer.lastCode(t)
	tok := tokenRe.FindStringSubmatch(page)[1]
	if strings.Contains(page, sent) || strings.Contains(tok, sent) {
		t.Fatal("code leaked into the page")
	}
	if code, _, _ := e.do("POST", "/verify", url.Values{"token": {tok}, "code": {"000000"}, "return": {"name=Ada"}}); code != 422 && sent != "000000" {
		t.Fatalf("wrong code: %d", code)
	}
	code, _, res := e.do("POST", "/verify", url.Values{"token": {tok}, "code": {sent[:3] + " " + sent[3:]}, "return": {"name=Ada"}})
	if code != 303 || res.Header.Get("Location") != "/form/wf/form?name=Ada" {
		t.Fatalf("verify: %d %q", code, res.Header.Get("Location"))
	}
	// A code works once.
	if code, _, _ := e.do("POST", "/verify", url.Values{"token": {tok}, "code": {sent}}); code != 410 {
		t.Fatalf("reused code: %d", code)
	}

	_, page, _ = e.do("GET", "?name=Ada", nil)
	if !strings.Contains(page, "ada@sub.acme.test") || !strings.Contains(page, `value="Ada"`) {
		t.Fatal("form should show the confirmed address and prefill")
	}
	if code, _, _ := e.do("POST", "", answers); code != 303 {
		t.Fatalf("submit: %d", code)
	}
	by := e.eng.reqs[0].Items[0][nodes.FormSubmittedBy].(map[string]any)
	if by["email"] != "ada@sub.acme.test" || by["sub"] != nil {
		t.Fatalf("submittedBy %v", by)
	}

	// "Not you?" forgets the address.
	e.do("POST", "/signout", url.Values{})
	if code, _, _ := e.do("POST", "", answers); code != 403 {
		t.Fatalf("after sign out: %d", code)
	}
}

func TestFormAccessCodeLimits(t *testing.T) {
	e := newFormEnv(t, map[string]any{"protection": "email"})
	_, page, _ := e.do("POST", "/code", url.Values{"email": {"a@b.test"}})
	tok := tokenRe.FindStringSubmatch(page)[1]
	sent := e.mailer.lastCode(t)
	bad := "111111"
	if sent == bad {
		bad = "222222"
	}
	for range codeTriesPerCode {
		e.do("POST", "/verify", url.Values{"token": {tok}, "code": {bad}})
	}
	if code, _, _ := e.do("POST", "/verify", url.Values{"token": {tok}, "code": {sent}}); code != 429 {
		t.Fatalf("brute force not limited: %d", code)
	}
	for range codesPerEmailHour {
		e.do("POST", "/code", url.Values{"email": {"c@b.test"}})
	}
	if code, _, _ := e.do("POST", "/code", url.Values{"email": {"c@b.test"}}); code != 429 {
		t.Fatalf("code requests not limited: %d", code)
	}
}

func TestFormAccessCodeFormCap(t *testing.T) {
	e := newFormEnv(t, map[string]any{"protection": "email"})
	// Each request comes from another address and e-mail, so only the
	// per-form cap can stop them.
	post := func(i int) int {
		form := url.Values{"email": {fmt.Sprintf("u%d@b.test", i)}}
		req, _ := http.NewRequest("POST", e.base+"/code", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Test-Peer", fmt.Sprintf("10.1.%d.%d:1000", i/250, i%250))
		res, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for i := range codesPerFormHour {
		if code := post(i); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if code := post(codesPerFormHour); code != 429 {
		t.Fatalf("form cap not enforced: %d", code)
	}
}

func TestFormAccessCodeNeedsMail(t *testing.T) {
	e := newFormEnv(t, map[string]any{"protection": "email"})
	e.mailer.configured = false
	if code, _, _ := e.do("GET", "", nil); code != 503 {
		t.Fatalf("status %d", code)
	}
}

func TestFormLogin(t *testing.T) {
	e := newFormEnv(t, map[string]any{"protection": "login"})
	code, _, res := e.do("GET", "?team=Sales", nil)
	loc, _ := url.Parse(res.Header.Get("Location"))
	if code != 302 || loc.Host != "idp.example" {
		t.Fatalf("expected redirect to the IdP: %d %v", code, loc)
	}
	if loc.Query().Get("redirect_uri") != e.base+"/callback" {
		t.Fatalf("redirect_uri %q", loc.Query().Get("redirect_uri"))
	}
	state := loc.Query().Get("state")

	if code, _, _ := e.do("GET", "/callback?code=good&state=forged", nil); code != 400 {
		t.Fatalf("forged state: %d", code)
	}
	// The failed attempt does not clear the login cookie, so the real one continues.
	code, _, res = e.do("GET", "/callback?code=good&state="+url.QueryEscape(state), nil)
	if code != 303 || res.Header.Get("Location") != "/form/wf/form?team=Sales" {
		t.Fatalf("callback: %d %q", code, res.Header.Get("Location"))
	}
	_, page, _ := e.do("GET", "", nil)
	if !strings.Contains(page, "Ada (ada@acme.test)") || strings.Contains(page, "Not you?") {
		t.Fatal("expected the signed-in user")
	}
	if code, _, _ := e.do("POST", "", answers); code != 303 {
		t.Fatalf("submit: %d", code)
	}
	by := e.eng.reqs[0].Items[0][nodes.FormSubmittedBy].(map[string]any)
	if by["username"] != "ada" || by["email"] != "ada@acme.test" || by["sub"] != "u1" {
		t.Fatalf("submittedBy %v", by)
	}
}

func TestFormLoginOtherOrg(t *testing.T) {
	e := newFormEnv(t, map[string]any{"protection": "login"})
	e.auth.user.Org = "other"
	_, _, res := e.do("GET", "", nil)
	loc, _ := url.Parse(res.Header.Get("Location"))
	if code, _, _ := e.do("GET", "/callback?code=good&state="+url.QueryEscape(loc.Query().Get("state")), nil); code != 403 {
		t.Fatalf("status %d", code)
	}
	if code, _, _ := e.do("POST", "", answers); code != 403 || len(e.eng.reqs) != 0 {
		t.Fatalf("submission from another org: %d", code)
	}
}

func TestFormLoginAuthDisabled(t *testing.T) {
	e := newFormEnv(t, map[string]any{"protection": "login"})
	e.auth.disabled = true
	if code, page, _ := e.do("GET", "", nil); code != 200 || !strings.Contains(page, "Answering as") {
		t.Fatalf("status %d", code)
	}
}

func TestFormTokensAreBound(t *testing.T) {
	s := newFormSealer("k")
	tok := s.seal(formClaims{Kind: tokSession, WF: "a", Node: "n"}, time.Hour, "login")
	if _, err := s.open(tok, tokSession, "b", "n", "login"); err == nil {
		t.Fatal("token valid for another workflow")
	}
	if _, err := s.open(tok, tokSession, "a", "n", "email"); err == nil {
		t.Fatal("session of another protection mode accepted")
	}
	if _, err := newFormSealer("other").open(tok, tokSession, "a", "n", "login"); err == nil {
		t.Fatal("token valid under another key")
	}
	old := s.seal(formClaims{Kind: tokSession, WF: "a", Node: "n"}, -time.Second, "login")
	if _, err := s.open(old, tokSession, "a", "n", "login"); err != errTokenExpired {
		t.Fatalf("expired token: %v", err)
	}
	_ = workflow.Workflow{}
}
