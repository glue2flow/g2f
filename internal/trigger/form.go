package trigger

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

const maxFormBody = 256 << 10 // 256 KiB, far above the per-field limits

const (
	sessionCookie = "g2f_form"
	loginCookie   = "g2f_form_login"
)

// FormURL returns the public URL of a form trigger.
func FormURL(publicURL, workflowID, nodeID string) string {
	return strings.TrimRight(publicURL, "/") + formPath(workflowID, nodeID)
}

func formPath(workflowID, nodeID string) string {
	return "/form/" + workflowID + "/" + url.PathEscape(nodeID)
}

// FormOptions configures a FormHandler.
type FormOptions struct {
	// SecretKey derives the key that signs captcha, access code and session tokens.
	SecretKey string
	PublicURL string
	// State keeps one-time tokens and rate limits; required for captcha and
	// access code protection.
	State FormState
	// Auth signs users in for login protection.
	Auth FormAuth
	// Mailer sends access codes.
	Mailer FormMailer
}

// FormHandler serves the pages of form triggers below /form/{workflowId}/{nodeId}:
//
//	GET  …            the form (or the sign-in / access code step first)
//	POST …            submit the answers, start the workflow
//	POST …/code       send an access code to an e-mail address
//	POST …/verify     check the access code
//	POST …/signout    forget the confirmed e-mail address
//	GET  …/callback   return from the Keycloak login
//
// Pages are plain HTML without JavaScript, so they work everywhere.
type FormHandler struct {
	store  Store
	eng    Engine
	reg    *nodes.Registry
	log    *slog.Logger
	opts   FormOptions
	sealer formSealer
	secure bool // cookies only over HTTPS
}

func NewFormHandler(st Store, eng Engine, reg *nodes.Registry, log *slog.Logger, opts FormOptions) *FormHandler {
	return &FormHandler{
		store: st, eng: eng, reg: reg, log: log.With("component", "form"), opts: opts,
		sealer: newFormSealer(opts.SecretKey),
		secure: strings.HasPrefix(opts.PublicURL, "https://"),
	}
}

// formReq is one request to a form page.
type formReq struct {
	w      http.ResponseWriter
	r      *http.Request
	wf     *workflow.Workflow
	nodeID string
	form   *nodes.Form
	base   string // path of the form
}

func (h *FormHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")

	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/form/"), "/"), "/")
	action := ""
	if len(parts) == 3 {
		action, parts = parts[2], parts[:2]
	}
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		h.message(w, http.StatusNotFound, "Form not found", "This link does not point to a form.")
		return
	}
	method := map[string]string{"": "", "code": http.MethodPost, "verify": http.MethodPost,
		"signout": http.MethodPost, "callback": http.MethodGet}
	want, known := method[action]
	if !known {
		h.message(w, http.StatusNotFound, "Form not found", "This link does not point to a form.")
		return
	}
	if (want == "" && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost) ||
		(want == http.MethodGet && r.Method != http.MethodGet && r.Method != http.MethodHead) ||
		(want == http.MethodPost && r.Method != http.MethodPost) {
		w.Header().Set("Allow", "GET, POST")
		h.message(w, http.StatusMethodNotAllowed, "Method not allowed", "Open this link in a browser.")
		return
	}

	fr, ok := h.load(w, r, parts[0], parts[1])
	if !ok {
		return
	}
	switch action {
	case "code":
		h.requestCode(fr)
	case "verify":
		h.verifyCode(fr)
	case "signout":
		h.clearCookie(fr, sessionCookie)
		http.Redirect(w, r, fr.base, http.StatusSeeOther)
	case "callback":
		h.loginCallback(fr)
	default:
		if r.Method == http.MethodPost {
			h.submit(fr)
		} else {
			h.show(fr)
		}
	}
}

func (h *FormHandler) load(w http.ResponseWriter, r *http.Request, wfID, nodeID string) (*formReq, bool) {
	wf, err := h.store.GetWorkflow(r.Context(), wfID)
	if errors.Is(err, store.ErrNotFound) {
		h.message(w, http.StatusNotFound, "Form not found", "This link does not point to a form.")
		return nil, false
	} else if err != nil {
		h.log.Error("load workflow", "err", err)
		h.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.")
		return nil, false
	}
	node := wf.Graph.Node(nodeID)
	if node == nil || node.Disabled || h.reg.TriggerKind(node.Type) != nodes.TriggerForm {
		h.message(w, http.StatusNotFound, "Form not found", "This link does not point to a form.")
		return nil, false
	}
	if !wf.Active {
		h.message(w, http.StatusNotFound, "Form closed", "This form is not accepting responses right now.")
		return nil, false
	}
	form, err := nodes.ParseForm(node.Params)
	if err == nil {
		err = h.protectionAvailable(r.Context(), wf, form)
	}
	if err != nil {
		h.log.Warn("form misconfigured", "workflow", wf.ID, "node", nodeID, "err", err)
		h.message(w, http.StatusServiceUnavailable, "Form unavailable", "This form is not set up correctly.")
		return nil, false
	}
	return &formReq{w: w, r: r, wf: wf, nodeID: nodeID, form: form, base: formPath(wf.ID, nodeID)}, true
}

// protectionAvailable checks that the server can provide the form's protection.
func (h *FormHandler) protectionAvailable(ctx context.Context, wf *workflow.Workflow, f *nodes.Form) error {
	switch f.Protection {
	case nodes.FormProtectCaptcha:
		if h.opts.State == nil {
			return errors.New("captcha needs the state store")
		}
	case nodes.FormProtectLogin:
		if h.opts.Auth == nil {
			return errors.New("login is not available")
		}
	case nodes.FormProtectEmail:
		if h.opts.State == nil || h.opts.Mailer == nil {
			return errors.New("access codes are not available")
		}
		ok, err := h.opts.Mailer.Configured(ctx, wf.OrgID)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("e-mail is not configured (Plugins → Email → Settings)")
		}
	}
	return nil
}

func (h *FormHandler) page(fr *formReq) formPage {
	return formPage{Form: fr.form, Base: fr.base, Action: fr.base}
}

// show handles GET: the success page, a protection step, or the form.
func (h *FormHandler) show(fr *formReq) {
	q := fr.r.URL.Query()
	if q.Has("submitted") {
		h.render(fr.w, http.StatusOK, formPage{Form: fr.form, Step: stepDone, Base: fr.base})
		return
	}
	p := h.page(fr)
	p.Values = q // query parameters prefill answers, e.g. ?team=Sales
	switch fr.form.Protection {
	case nodes.FormProtectCaptcha:
		p.Captcha = h.newCaptcha(fr)
	case nodes.FormProtectLogin:
		user := h.identity(fr)
		if user == nil {
			h.startLogin(fr)
			return
		}
		p.Identity = user.label()
	case nodes.FormProtectEmail:
		user := h.identity(fr)
		if user == nil {
			h.render(fr.w, http.StatusOK, formPage{Form: fr.form, Step: stepEmail, Base: fr.base, Return: q.Encode()})
			return
		}
		p.Identity, p.SignOut = user.label(), true
	}
	h.render(fr.w, http.StatusOK, p)
}

func (h *FormHandler) newCaptcha(fr *formReq) *captchaView {
	q, tok := h.sealer.newCaptcha(fr.wf.ID, fr.nodeID)
	return &captchaView{Question: q, Token: tok}
}

// submit handles POST of the answers.
func (h *FormHandler) submit(fr *formReq) {
	w, r := fr.w, fr.r
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		h.message(w, http.StatusRequestEntityTooLarge, "Response too large", "Please shorten your answers and try again.")
		return
	}
	p := h.page(fr)
	p.Values = r.PostForm

	var by *submitter
	switch fr.form.Protection {
	case nodes.FormProtectLogin, nodes.FormProtectEmail:
		if by = h.identity(fr); by == nil {
			h.message(w, http.StatusForbidden, "Session expired",
				"Please open the form again to confirm who you are. Your answers were not recorded.")
			return
		}
		p.Identity, p.SignOut = by.label(), fr.form.Protection == nodes.FormProtectEmail
	}

	item, errs := fr.form.Submit(r.PostForm)
	if fr.form.Protection == nodes.FormProtectCaptcha {
		tok, answer := r.PostForm.Get("_captcha"), strings.TrimSpace(r.PostForm.Get("_answer"))
		if n, err := strconv.Atoi(strings.ReplaceAll(answer, "−", "-")); err == nil {
			answer = strconv.Itoa(n) // "019" → "19"
		}
		if errs != nil {
			// Keep the question until the other answers are fixed.
			if c, _, _, err := peek(tok); err == nil {
				p.Captcha = &captchaView{Question: c.Question, Token: tok, Answer: answer}
			}
		} else if msg := h.checkCaptcha(fr, tok, answer); msg != "" {
			errs = map[string]string{}
			p.Captcha = h.newCaptcha(fr)
			p.Captcha.Error = msg
		}
		if p.Captcha == nil {
			p.Captcha = h.newCaptcha(fr)
		}
	}
	if errs != nil {
		p.Errors = errs
		h.render(w, http.StatusUnprocessableEntity, p)
		return
	}
	if by != nil {
		item[nodes.FormSubmittedBy] = by.item()
	}
	exec, err := h.eng.Start(r.Context(), engine.Request{
		Workflow:    fr.wf,
		TriggerType: nodes.TriggerForm,
		TriggerNode: fr.nodeID,
		TriggeredBy: "form",
		Items:       []nodes.Item{item},
	})
	if err != nil {
		h.log.Error("start form workflow", "workflow", fr.wf.ID, "err", err)
		p.Failure = "Your response could not be recorded. Please try again later."
		if fr.form.Protection == nodes.FormProtectCaptcha {
			p.Captcha = h.newCaptcha(fr)
		}
		h.render(w, http.StatusInternalServerError, p)
		return
	}
	w.Header().Set("X-G2F-Execution-Id", exec.ID)
	// Post/Redirect/Get, so reloading the page does not submit twice.
	http.Redirect(w, r, fr.base+"?submitted", http.StatusSeeOther)
}

// checkCaptcha returns an error message, or "" when the answer is right.
// Each question can be answered once, right or wrong.
func (h *FormHandler) checkCaptcha(fr *formReq, tok, answer string) string {
	c, _, _, err := peek(tok)
	if err != nil {
		return "Please answer the question."
	}
	fresh, err := h.opts.State.StateRemember(fr.r.Context(), fr.wf.ID, stateScope(fr.nodeID), []string{"captcha:" + c.Nonce}, captchaTTL+time.Minute)
	if err != nil {
		h.log.Error("captcha state", "err", err)
		return "Please try again."
	}
	if !fresh[0] {
		return "This question was already answered. Please answer the new one."
	}
	if _, err := h.sealer.open(tok, tokCaptcha, fr.wf.ID, fr.nodeID, answer); err != nil {
		if errors.Is(err, errTokenExpired) {
			return "The question expired. Please answer the new one."
		}
		return "That was not right. Please try again."
	}
	return ""
}

func stateScope(nodeID string) string { return "_form:" + nodeID }

// identity returns the signed-in user or the confirmed e-mail address.
func (h *FormHandler) identity(fr *formReq) *submitter {
	if fr.form.Protection == nodes.FormProtectLogin && h.opts.Auth.Disabled() {
		u := h.opts.Auth.DevUser()
		return &submitter{Sub: u.Sub, Username: u.Username, Name: u.Name, Email: u.Email, Org: u.Org}
	}
	ck, err := fr.r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	c, err := h.sealer.open(ck.Value, tokSession, fr.wf.ID, fr.nodeID, fr.form.Protection)
	if err != nil || c.User == nil {
		return nil
	}
	if fr.form.Protection == nodes.FormProtectLogin && c.User.Org != fr.wf.OrgID {
		return nil
	}
	return c.User
}

func (h *FormHandler) setCookie(fr *formReq, name, value string, ttl time.Duration) {
	http.SetCookie(fr.w, &http.Cookie{
		Name: name, Value: value, Path: fr.base, MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (h *FormHandler) clearCookie(fr *formReq, name string) {
	http.SetCookie(fr.w, &http.Cookie{Name: name, Value: "", Path: fr.base, MaxAge: -1,
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode})
}

func (h *FormHandler) startSession(fr *formReq, user *submitter) {
	tok := h.sealer.seal(formClaims{Kind: tokSession, WF: fr.wf.ID, Node: fr.nodeID, User: user}, formSessionTTL, fr.form.Protection)
	h.setCookie(fr, sessionCookie, tok, formSessionTTL)
}

// returnTo is the form URL with a (re-encoded) prefill query.
func returnTo(base, query string) string {
	if v, err := url.ParseQuery(query); err == nil && len(v) > 0 {
		return base + "?" + v.Encode()
	}
	return base
}

// --- Login protection ---------------------------------------------------------

func (h *FormHandler) callbackURL(fr *formReq) string {
	return strings.TrimRight(h.opts.PublicURL, "/") + fr.base + "/callback"
}

func (h *FormHandler) startLogin(fr *formReq) {
	c := formClaims{Kind: tokLogin, WF: fr.wf.ID, Node: fr.nodeID, State: randomHex(16), Verifier: randomHex(32),
		Return: fr.r.URL.Query().Encode()}
	h.setCookie(fr, loginCookie, h.sealer.seal(c, loginStateTTL, ""), loginStateTTL)
	http.Redirect(fr.w, fr.r, h.opts.Auth.LoginURL(h.callbackURL(fr), c.State, c.Verifier), http.StatusFound)
}

func (h *FormHandler) loginCallback(fr *formReq) {
	w, r := fr.w, fr.r
	if fr.form.Protection != nodes.FormProtectLogin {
		http.Redirect(w, r, fr.base, http.StatusFound)
		return
	}
	q := r.URL.Query()
	ck, err := r.Cookie(loginCookie)
	var c *formClaims
	if err == nil {
		c, err = h.sealer.open(ck.Value, tokLogin, fr.wf.ID, fr.nodeID, "")
	}
	if err != nil || subtle.ConstantTimeCompare([]byte(c.State), []byte(q.Get("state"))) != 1 {
		h.message(w, http.StatusBadRequest, "Sign-in failed", "The sign-in took too long or was started elsewhere. Please open the form again.")
		return
	}
	h.clearCookie(fr, loginCookie)
	if e := q.Get("error"); e != "" || q.Get("code") == "" {
		h.message(w, http.StatusUnauthorized, "Sign-in failed", "You need to sign in to answer this form.")
		return
	}
	u, err := h.opts.Auth.Exchange(r.Context(), q.Get("code"), c.Verifier, h.callbackURL(fr))
	if err != nil {
		h.log.Warn("form login", "workflow", fr.wf.ID, "err", err)
		h.message(w, http.StatusUnauthorized, "Sign-in failed", "Please open the form again.")
		return
	}
	if u.Org != fr.wf.OrgID {
		h.message(w, http.StatusForbidden, "No access", "This form is only open to members of another organization.")
		return
	}
	h.startSession(fr, &submitter{Sub: u.Sub, Username: u.Username, Name: u.Name, Email: u.Email, Org: u.Org})
	http.Redirect(w, r, returnTo(fr.base, c.Return), http.StatusSeeOther)
}

// --- Access code protection ---------------------------------------------------

func (h *FormHandler) emailOnly(fr *formReq) bool {
	if fr.form.Protection != nodes.FormProtectEmail {
		http.Redirect(fr.w, fr.r, fr.base, http.StatusSeeOther)
		return false
	}
	fr.r.Body = http.MaxBytesReader(fr.w, fr.r.Body, 16<<10)
	if err := fr.r.ParseForm(); err != nil {
		h.message(fr.w, http.StatusBadRequest, "Invalid request", "Please open the form again.")
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// requestCode sends an access code to the entered address.
func (h *FormHandler) requestCode(fr *formReq) {
	if !h.emailOnly(fr) {
		return
	}
	ctx, r := fr.r.Context(), fr.r
	p := formPage{Form: fr.form, Step: stepEmail, Base: fr.base, Return: r.PostForm.Get("return")}
	raw := strings.TrimSpace(r.PostForm.Get("email"))
	p.Email = raw
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Name != "" || len(raw) > 254 || !strings.Contains(addr.Address[strings.LastIndex(addr.Address, "@")+1:], ".") {
		p.EmailError = "Please enter a valid e-mail address."
		h.render(fr.w, http.StatusUnprocessableEntity, p)
		return
	}
	email := strings.ToLower(addr.Address)
	if !fr.form.EmailAllowed(email) {
		p.EmailError = "Please use an address of " + strings.Join(fr.form.EmailDomains, ", ") + "."
		h.render(fr.w, http.StatusUnprocessableEntity, p)
		return
	}
	scope := stateScope(fr.nodeID)
	for _, lim := range []struct {
		key string
		max float64
	}{{"codes:email:" + email, codesPerEmailHour}, {"codes:ip:" + clientIP(r), codesPerClientHour}} {
		n, err := h.opts.State.StateIncrement(ctx, fr.wf.ID, scope, lim.key, 1, time.Hour)
		if err != nil {
			h.log.Error("form rate limit", "err", err)
			h.message(fr.w, http.StatusInternalServerError, "Something went wrong", "Please try again later.")
			return
		}
		if n > lim.max {
			p.EmailError = "Too many codes were requested. Please try again in an hour."
			h.render(fr.w, http.StatusTooManyRequests, p)
			return
		}
	}
	code, tok := h.sealer.newCode(fr.wf.ID, fr.nodeID, email)
	text := "Your access code for \"" + fr.form.Title + "\" is:\n\n    " + code + "\n\n" +
		"It is valid for " + strconv.Itoa(int(codeTTL.Minutes())) + " minutes. If you did not request it, you can ignore this e-mail.\n"
	if err := h.opts.Mailer.SendText(ctx, fr.wf.OrgID, email, "Access code: "+code, text); err != nil {
		h.log.Error("send form access code", "workflow", fr.wf.ID, "err", err)
		p.EmailError = "The e-mail could not be sent. Please try again later."
		h.render(fr.w, http.StatusBadGateway, p)
		return
	}
	h.render(fr.w, http.StatusOK, formPage{Form: fr.form, Step: stepCode, Base: fr.base, Email: email,
		CodeToken: tok, Return: p.Return})
}

// verifyCode checks an access code and remembers the address in a cookie.
func (h *FormHandler) verifyCode(fr *formReq) {
	if !h.emailOnly(fr) {
		return
	}
	ctx, r := fr.r.Context(), fr.r
	tok, code := r.PostForm.Get("token"), strings.Join(strings.Fields(r.PostForm.Get("code")), "")
	p := formPage{Form: fr.form, Step: stepCode, Base: fr.base, CodeToken: tok, Return: r.PostForm.Get("return")}
	c, _, _, err := peek(tok)
	if err != nil {
		http.Redirect(fr.w, r, fr.base, http.StatusSeeOther)
		return
	}
	p.Email = c.Email
	scope := stateScope(fr.nodeID)
	tries, err := h.opts.State.StateIncrement(ctx, fr.wf.ID, scope, "code-tries:"+c.Nonce, 1, codeTTL+time.Minute)
	if err != nil {
		h.log.Error("form code attempts", "err", err)
		h.message(fr.w, http.StatusInternalServerError, "Something went wrong", "Please try again later.")
		return
	}
	if tries > codeTriesPerCode {
		h.render(fr.w, http.StatusTooManyRequests, formPage{Form: fr.form, Step: stepEmail, Base: fr.base, Email: c.Email,
			Return: p.Return, EmailError: "Too many wrong codes. Please request a new one."})
		return
	}
	c, err = h.sealer.open(tok, tokCode, fr.wf.ID, fr.nodeID, code)
	switch {
	case errors.Is(err, errTokenExpired):
		h.render(fr.w, http.StatusGone, formPage{Form: fr.form, Step: stepEmail, Base: fr.base, Email: p.Email,
			Return: p.Return, EmailError: "The code expired. Please request a new one."})
		return
	case err != nil:
		p.CodeError = "This code is not correct."
		h.render(fr.w, http.StatusUnprocessableEntity, p)
		return
	}
	if fresh, err := h.opts.State.StateRemember(ctx, fr.wf.ID, scope, []string{"code:" + c.Nonce}, codeTTL+time.Minute); err != nil || !fresh[0] {
		h.render(fr.w, http.StatusGone, formPage{Form: fr.form, Step: stepEmail, Base: fr.base, Email: c.Email,
			Return: p.Return, EmailError: "This code was already used. Please request a new one."})
		return
	}
	h.startSession(fr, &submitter{Email: c.Email})
	http.Redirect(fr.w, r, returnTo(fr.base, p.Return), http.StatusSeeOther)
}

func (h *FormHandler) message(w http.ResponseWriter, status int, heading, text string) {
	h.render(w, status, formPage{Step: stepMessage, Heading: heading, Text: text})
}

func (h *FormHandler) render(w http.ResponseWriter, status int, p formPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := formTemplate.Execute(w, p); err != nil {
		h.log.Error("render form", "err", err)
	}
}
