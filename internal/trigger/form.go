package trigger

import (
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
)

const maxFormBody = 256 << 10 // 256 KiB, far above the per-field limits

// FormURL returns the public URL of a form trigger.
func FormURL(publicURL, workflowID, nodeID string) string {
	return strings.TrimRight(publicURL, "/") + "/form/" + workflowID + "/" + url.PathEscape(nodeID)
}

// FormHandler serves GET/POST /form/{workflowId}/{nodeId}: GET shows the form
// of a form trigger, POST validates the answers and starts the workflow.
// Pages are plain HTML without JavaScript, so they work everywhere.
type FormHandler struct {
	store Store
	eng   Engine
	reg   *nodes.Registry
	log   *slog.Logger
}

func NewFormHandler(st Store, eng Engine, reg *nodes.Registry, log *slog.Logger) *FormHandler {
	return &FormHandler{store: st, eng: eng, reg: reg, log: log.With("component", "form")}
}

func (h *FormHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")

	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/form/"), "/"), "/")
	if len(parts) != 2 {
		h.message(w, http.StatusNotFound, "Form not found", "This link does not point to a form.")
		return
	}
	wfID, nodeID := parts[0], parts[1]
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		h.message(w, http.StatusMethodNotAllowed, "Method not allowed", "Open this link in a browser.")
		return
	}

	wf, err := h.store.GetWorkflow(r.Context(), wfID)
	if errors.Is(err, store.ErrNotFound) {
		h.message(w, http.StatusNotFound, "Form not found", "This link does not point to a form.")
		return
	} else if err != nil {
		h.log.Error("load workflow", "err", err)
		h.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.")
		return
	}
	node := wf.Graph.Node(nodeID)
	if node == nil || node.Disabled || h.reg.TriggerKind(node.Type) != nodes.TriggerForm {
		h.message(w, http.StatusNotFound, "Form not found", "This link does not point to a form.")
		return
	}
	if !wf.Active {
		h.message(w, http.StatusNotFound, "Form closed", "This form is not accepting responses right now.")
		return
	}
	form, err := nodes.ParseForm(node.Params)
	if err != nil {
		h.log.Warn("form misconfigured", "workflow", wf.ID, "node", nodeID, "err", err)
		h.message(w, http.StatusInternalServerError, "Form unavailable", "This form is not set up correctly.")
		return
	}

	if r.Method != http.MethodPost {
		if r.URL.Query().Has("submitted") {
			h.render(w, http.StatusOK, formPage{Form: form, Done: true})
			return
		}
		// Query parameters prefill answers, e.g. ?team=Sales.
		h.render(w, http.StatusOK, formPage{Form: form, Values: r.URL.Query(), Action: r.URL.Path})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		h.message(w, http.StatusRequestEntityTooLarge, "Response too large", "Please shorten your answers and try again.")
		return
	}
	item, errs := form.Submit(r.PostForm)
	if errs != nil {
		h.render(w, http.StatusUnprocessableEntity, formPage{Form: form, Values: r.PostForm, Errors: errs, Action: r.URL.Path})
		return
	}
	exec, err := h.eng.Start(r.Context(), engine.Request{
		Workflow:    wf,
		TriggerType: nodes.TriggerForm,
		TriggerNode: nodeID,
		TriggeredBy: "form",
		Items:       []nodes.Item{item},
	})
	if err != nil {
		h.log.Error("start form workflow", "workflow", wf.ID, "err", err)
		h.render(w, http.StatusInternalServerError, formPage{Form: form, Values: r.PostForm, Action: r.URL.Path,
			Failure: "Your response could not be recorded. Please try again later."})
		return
	}
	w.Header().Set("X-G2F-Execution-Id", exec.ID)
	// Post/Redirect/Get, so reloading the page does not submit twice.
	http.Redirect(w, r, r.URL.Path+"?submitted", http.StatusSeeOther)
}

type formPage struct {
	Form    *nodes.Form
	Values  url.Values
	Errors  map[string]string
	Action  string
	Done    bool
	Failure string
	// Message pages (errors) have no form.
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

func (h *FormHandler) message(w http.ResponseWriter, status int, heading, text string) {
	h.render(w, status, formPage{Heading: heading, Text: text})
}

func (h *FormHandler) render(w http.ResponseWriter, status int, p formPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := formTemplate.Execute(w, p); err != nil {
		h.log.Error("render form", "err", err)
	}
}

var formTemplate = template.Must(template.New("form").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{if .Form}}{{.Form.Title}}{{else}}{{.Heading}}{{end}}</title>
<style>
:root { --bg:#f4f6f9; --card:#fff; --text:#1f2933; --muted:#616e7c; --border:#d3d9e0; --accent:#0ea5e9; --accent-text:#fff; --error:#c81e1e; --error-bg:#fdecec; }
@media (prefers-color-scheme: dark) {
  :root { --bg:#111418; --card:#1b2027; --text:#e6e9ee; --muted:#9aa5b1; --border:#343b45; --accent:#38bdf8; --accent-text:#06202c; --error:#f87171; --error-bg:#3a1d1d; }
}
:root { color-scheme: light dark; }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--text); font:16px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif; }
main { max-width:640px; margin:0 auto; padding:32px 16px 48px; }
.card { background:var(--card); border:1px solid var(--border); border-radius:12px; padding:24px; margin-bottom:16px; }
.head { border-top:6px solid var(--accent); }
h1 { margin:0 0 8px; font-size:1.6rem; line-height:1.25; }
p { margin:0 0 8px; white-space:pre-line; }
.muted { color:var(--muted); }
.note { font-size:.85rem; margin-top:12px; }
fieldset { border:0; margin:0; padding:0; min-width:0; }
legend { padding:0; }
.q { display:block; font-weight:600; margin-bottom:4px; }
.req { color:var(--error); margin-left:2px; }
.help { color:var(--muted); font-size:.9rem; margin-bottom:8px; }
input[type=text], textarea { width:100%; padding:10px 12px; border:1px solid var(--border); border-radius:8px; background:transparent; color:inherit; font:inherit; margin-top:4px; }
textarea { min-height:110px; resize:vertical; }
input:focus, textarea:focus { outline:2px solid var(--accent); outline-offset:1px; border-color:transparent; }
.opt { display:flex; gap:10px; align-items:flex-start; padding:6px 0; cursor:pointer; }
.opt input { width:18px; height:18px; margin:3px 0 0; accent-color:var(--accent); flex:none; }
.invalid { border-color:var(--error); }
.err { color:var(--error); font-size:.9rem; margin-top:6px; }
.banner { background:var(--error-bg); color:var(--error); border-radius:8px; padding:10px 12px; margin-bottom:16px; }
button { background:var(--accent); color:var(--accent-text); border:0; border-radius:8px; padding:12px 24px; font:inherit; font-weight:600; cursor:pointer; }
button:hover { filter:brightness(1.08); }
.foot { text-align:center; font-size:.8rem; color:var(--muted); }
a { color:var(--accent); }
</style>
</head>
<body>
<main>
{{- if not .Form}}
  <div class="card head"><h1>{{.Heading}}</h1><p class="muted">{{.Text}}</p></div>
{{- else if .Done}}
  <div class="card head">
    <h1>{{.Form.Title}}</h1>
    {{range .Paragraphs .Form.SuccessMessage}}<p>{{.}}</p>{{end}}
    <p><a href="?">Submit another response</a></p>
  </div>
{{- else}}
  {{$p := .}}
  <div class="card head">
    <h1>{{.Form.Title}}</h1>
    {{range .Paragraphs .Form.Description}}<p class="muted">{{.}}</p>{{end}}
    {{if .HasRequired}}<p class="muted note"><span class="req">*</span> Required</p>{{end}}
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
    <button type="submit">{{.Form.SubmitLabel}}</button>
  </form>
{{- end}}
  <p class="foot">Powered by glue2flow</p>
</main>
</body>
</html>
`))
