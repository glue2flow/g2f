package trigger

import (
	"html/template"
	"net/url"
	"slices"
	"strings"

	"github.com/glue2flow/g2f/internal/nodes"
)

// Steps of the form pages.
const (
	stepForm    = ""        // the questions
	stepDone    = "done"    // after submitting
	stepEmail   = "email"   // enter an address for an access code
	stepCode    = "code"    // enter the access code
	stepMessage = "message" // errors, closed forms
)

type captchaView struct {
	Question string
	Token    string
	Answer   string
	Error    string
}

type formPage struct {
	Step    string
	Form    *nodes.Form
	Base    string // path of the form
	Action  string
	Values  url.Values
	Errors  map[string]string
	Failure string
	Captcha *captchaView
	// Identity is who answers (login or confirmed address); SignOut offers to change it.
	Identity string
	SignOut  bool
	// Access code steps.
	Email      string
	EmailError string
	CodeToken  string
	CodeError  string
	Return     string // prefill query to restore after the access code step
	// Message pages.
	Heading string
	Text    string
}

func (p formPage) Value(key string) string { return p.Values.Get(key) }

func (p formPage) Checked(key, option string) bool { return slices.Contains(p.Values[key], option) }

func (p formPage) HasRequired() bool {
	return slices.ContainsFunc(p.Form.Fields, func(f nodes.FormField) bool { return f.Required })
}

func (p formPage) Paragraphs(s string) []string {
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		if para = strings.TrimSpace(para); para != "" {
			out = append(out, para)
		}
	}
	return out
}

func (p formPage) Title() string {
	if p.Form != nil {
		return p.Form.Title
	}
	return p.Heading
}

var formTemplate = template.Must(template.New("form").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light dark; --bg:#f4f6f9; --card:#fff; --text:#1f2933; --muted:#616e7c; --border:#d3d9e0; --accent:#0ea5e9; --accent-text:#fff; --error:#c81e1e; --error-bg:#fdecec; }
@media (prefers-color-scheme: dark) {
  :root { --bg:#111418; --card:#1b2027; --text:#e6e9ee; --muted:#9aa5b1; --border:#343b45; --accent:#38bdf8; --accent-text:#06202c; --error:#f87171; --error-bg:#3a1d1d; }
}
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--text); font:16px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif; }
main { max-width:640px; margin:0 auto; padding:32px 16px 48px; }
.card { background:var(--card); border:1px solid var(--border); border-radius:12px; padding:24px; margin-bottom:16px; }
.head { border-top:6px solid var(--accent); }
h1 { margin:0 0 8px; font-size:1.6rem; line-height:1.25; }
p { margin:0 0 8px; white-space:pre-line; }
.muted { color:var(--muted); }
.note { font-size:.85rem; margin-top:12px; }
.who { display:flex; flex-wrap:wrap; gap:4px 12px; align-items:center; font-size:.9rem; color:var(--muted); margin-top:12px; padding-top:12px; border-top:1px solid var(--border); }
.who form { display:inline; }
.link { background:none; border:0; padding:0; color:var(--accent); font:inherit; cursor:pointer; text-decoration:underline; }
fieldset { border:0; margin:0; padding:0; min-width:0; }
legend { padding:0; }
.q { display:block; font-weight:600; margin-bottom:4px; }
.req { color:var(--error); margin-left:2px; }
.help { color:var(--muted); font-size:.9rem; margin-bottom:8px; }
input[type=text], input[type=email], textarea { width:100%; padding:10px 12px; border:1px solid var(--border); border-radius:8px; background:transparent; color:inherit; font:inherit; margin-top:4px; }
textarea { min-height:110px; resize:vertical; }
input.short { width:8em; }
input.code { width:9em; letter-spacing:.3em; font-size:1.3rem; text-align:center; }
input:focus, textarea:focus { outline:2px solid var(--accent); outline-offset:1px; border-color:transparent; }
.opt { display:flex; gap:10px; align-items:flex-start; padding:6px 0; cursor:pointer; }
.opt input { width:18px; height:18px; margin:3px 0 0; accent-color:var(--accent); flex:none; }
.invalid { border-color:var(--error); }
.err { color:var(--error); font-size:.9rem; margin-top:6px; }
.banner { background:var(--error-bg); color:var(--error); border-radius:8px; padding:10px 12px; margin-bottom:16px; }
.captcha { font-size:1.15rem; font-weight:600; margin:4px 0; }
.row { display:flex; gap:10px; align-items:center; flex-wrap:wrap; margin-top:12px; }
button.primary { background:var(--accent); color:var(--accent-text); border:0; border-radius:8px; padding:12px 24px; font:inherit; font-weight:600; cursor:pointer; }
button.primary:hover { filter:brightness(1.08); }
.foot { text-align:center; font-size:.8rem; color:var(--muted); }
a { color:var(--accent); }
</style>
</head>
<body>
<main>
{{- if eq .Step "message"}}
  <div class="card head"><h1>{{.Heading}}</h1><p class="muted">{{.Text}}</p></div>
{{- else if eq .Step "done"}}
  <div class="card head">
    <h1>{{.Form.Title}}</h1>
    {{range .Paragraphs .Form.SuccessMessage}}<p>{{.}}</p>{{end}}
    <p><a href="{{.Base}}">Submit another response</a></p>
  </div>
{{- else if eq .Step "email"}}
  <div class="card head">
    <h1>{{.Form.Title}}</h1>
    {{range .Paragraphs .Form.Description}}<p class="muted">{{.}}</p>{{end}}
  </div>
  <form class="card" method="post" action="{{.Base}}/code" novalidate>
    <label class="q" for="email">Your e-mail address</label>
    <div class="help">We will send you an access code to open the form.</div>
    <input type="email" id="email" name="email" autocomplete="email" required value="{{.Email}}"{{if .EmailError}} class="invalid" aria-invalid="true"{{end}}>
    {{if .EmailError}}<div class="err" role="alert">{{.EmailError}}</div>{{end}}
    <input type="hidden" name="return" value="{{.Return}}">
    <div class="row"><button class="primary" type="submit">Send code</button></div>
  </form>
{{- else if eq .Step "code"}}
  <div class="card head">
    <h1>{{.Form.Title}}</h1>
    <p class="muted">We sent an access code to <strong>{{.Email}}</strong>. It is valid for 15 minutes.</p>
  </div>
  <form class="card" method="post" action="{{.Base}}/verify" novalidate>
    <label class="q" for="code">Access code</label>
    <input type="text" id="code" name="code" class="code{{if .CodeError}} invalid{{end}}" inputmode="numeric" autocomplete="one-time-code" maxlength="12" required autofocus{{if .CodeError}} aria-invalid="true"{{end}}>
    {{if .CodeError}}<div class="err" role="alert">{{.CodeError}}</div>{{end}}
    <input type="hidden" name="token" value="{{.CodeToken}}">
    <input type="hidden" name="return" value="{{.Return}}">
    <div class="row"><button class="primary" type="submit">Continue</button></div>
  </form>
  <p class="muted" style="text-align:center"><a href="{{.Base}}">Use a different address or send a new code</a></p>
{{- else}}
  {{$p := .}}
  <div class="card head">
    <h1>{{.Form.Title}}</h1>
    {{range .Paragraphs .Form.Description}}<p class="muted">{{.}}</p>{{end}}
    {{if .HasRequired}}<p class="muted note"><span class="req">*</span> Required</p>{{end}}
    {{if .Identity}}<div class="who"><span>Answering as <strong>{{.Identity}}</strong></span>{{if .SignOut}}<form method="post" action="{{.Base}}/signout"><button class="link" type="submit">Not you?</button></form>{{end}}</div>{{end}}
  </div>
  <form method="post" action="{{.Action}}" novalidate>
    {{if .Failure}}<div class="banner" role="alert">{{.Failure}}</div>{{else if .Errors}}<div class="banner" role="alert">Please check your answers below.</div>{{end}}
    {{range .Form.Fields}}
    {{$err := index $p.Errors .Key}}
    <div class="card">
    {{if or (eq .Type "choice") (eq .Type "multichoice")}}
      <fieldset{{if $err}} aria-invalid="true"{{end}}>
        <legend class="q">{{.Label}}{{if .Required}}<span class="req" aria-hidden="true">*</span>{{end}}</legend>
        {{if .Help}}<div class="help">{{.Help}}</div>{{end}}
        {{$f := .}}
        {{range $i, $o := .Options}}
        <label class="opt"><input type="{{if eq $f.Type "choice"}}radio{{else}}checkbox{{end}}" name="{{$f.Key}}" value="{{$o}}"{{if $p.Checked $f.Key $o}} checked{{end}}{{if and $f.Required (eq $f.Type "choice")}} required{{end}}> <span>{{$o}}</span></label>
        {{end}}
      </fieldset>
    {{else}}
      <label class="q" for="f-{{.Key}}">{{.Label}}{{if .Required}}<span class="req" aria-hidden="true">*</span>{{end}}</label>
      {{if .Help}}<div class="help">{{.Help}}</div>{{end}}
      {{if eq .Type "textarea"}}
      <textarea id="f-{{.Key}}" name="{{.Key}}" maxlength="10000" placeholder="{{.Placeholder}}"{{if .Required}} required{{end}}{{if $err}} class="invalid" aria-invalid="true"{{end}}>{{$p.Value .Key}}</textarea>
      {{else}}
      <input type="text" id="f-{{.Key}}" name="{{.Key}}" maxlength="1000" placeholder="{{.Placeholder}}" value="{{$p.Value .Key}}"{{if .Required}} required{{end}}{{if $err}} class="invalid" aria-invalid="true"{{end}}>
      {{end}}
    {{end}}
      {{if $err}}<div class="err">{{$err}}</div>{{end}}
    </div>
    {{end}}
    {{with .Captcha}}
    <div class="card">
      <label class="q" for="captcha">Please solve this to show you are human<span class="req" aria-hidden="true">*</span></label>
      <div class="captcha">{{.Question}} = ?</div>
      <input type="text" id="captcha" name="_answer" class="short{{if .Error}} invalid{{end}}" inputmode="numeric" autocomplete="off" maxlength="4" required value="{{.Answer}}"{{if .Error}} aria-invalid="true"{{end}}>
      <input type="hidden" name="_captcha" value="{{.Token}}">
      {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
    </div>
    {{end}}
    <button class="primary" type="submit">{{.Form.SubmitLabel}}</button>
  </form>
{{- end}}
  <p class="foot">Powered by glue2flow</p>
</main>
</body>
</html>
`))
