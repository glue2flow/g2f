package nodes

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/glue2flow/g2f/internal/workflow"
)

// --- Form trigger -----------------------------------------------------------

// Form field types.
const (
	FormText        = "text"        // single line
	FormTextarea    = "textarea"    // multiple lines
	FormChoice      = "choice"      // one of the options (radio buttons)
	FormMultiChoice = "multichoice" // any of the options (checkboxes)
)

// Limits that keep a public form from flooding workflows.
const (
	maxFormFields     = 50
	maxFormOptions    = 50
	maxFormTextLen    = 1000
	maxFormTextareaLn = 10000
)

// Output fields added to the answers; question keys may not use them.
const (
	FormSubmittedAt = "submittedAt"
	FormSubmittedBy = "submittedBy"
)

// Form protection modes: who may answer.
const (
	FormProtectNone    = "none"
	FormProtectCaptcha = "captcha" // a simple arithmetic question
	FormProtectLogin   = "login"   // sign in with the organization's Keycloak
	FormProtectEmail   = "email"   // confirm an e-mail address with a one-time code
)

var formKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type formTrigger struct{}

func defaultFormFields() []any {
	return []any{
		map[string]any{"key": "name", "label": "Your name", "type": FormText, "required": true},
		map[string]any{"key": "rating", "label": "How satisfied are you?", "type": FormChoice, "required": true,
			"options": []any{"Very satisfied", "Satisfied", "Unsatisfied"}},
		map[string]any{"key": "comment", "label": "Anything else?", "type": FormTextarea},
	}
}

func (formTrigger) Definition() Definition {
	return Definition{
		Type: "trigger.form", Name: "Form Trigger", Category: "Triggers", Source: "builtin", Module: BuiltinModule,
		Description: "Publish a simple web form (text and multiple-choice questions). Each submission starts the workflow. Only active workflows accept submissions.",
		Color:       "#0ea5e9", Trigger: TriggerForm, Inputs: []string{}, Outputs: []string{"main"},
		Params: []Param{
			{Name: "title", Label: "Form title", Type: ParamString, Default: "Feedback", NoExpression: true, Required: true},
			{Name: "description", Label: "Description", Type: ParamText, NoExpression: true,
				Description: "Shown under the title. Plain text; blank lines start a new paragraph."},
			{Name: "fields", Label: "Questions", Type: ParamFormFields, NoExpression: true, Required: true,
				Default:     defaultFormFields(),
				Description: "Each answer becomes a field of the output item, named by the question's key."},
			{Name: "protection", Label: "Protection", Type: ParamSelect, Default: FormProtectNone, NoExpression: true, Options: []Option{
				{"None – anyone with the link", FormProtectNone},
				{"Simple captcha (e.g. 12 + 7 = ?)", FormProtectCaptcha},
				{"Sign in with Keycloak (organization members)", FormProtectLogin},
				{"Access code sent by e-mail", FormProtectEmail},
			}, Description: "Login adds submittedBy (sub, username, name, email); access codes add submittedBy.email. Access codes need Plugins → Email → Settings."},
			{Name: "emailDomains", Label: "Allowed e-mail domains", Type: ParamString, NoExpression: true,
				ShowIf:      map[string][]string{"protection": {FormProtectEmail}},
				Placeholder: "example.com, example.org",
				Description: "Optional. Only addresses of these domains (and their subdomains) can request a code."},
			{Name: "submitLabel", Label: "Submit button", Type: ParamString, Default: "Submit", NoExpression: true},
			{Name: "successMessage", Label: "Message after submitting", Type: ParamText, NoExpression: true,
				Default: "Thank you! Your response has been recorded."},
		},
		OutputSchema: &OutputSchema{FormFieldsParam: "fields", Fields: fields(
			FormSubmittedAt+":string:Time the form was submitted (RFC 3339, UTC)",
			FormSubmittedBy+":object:Who answered (login or access code protection)",
			FormSubmittedBy+".email:string:E-mail address (login or access code)",
			FormSubmittedBy+".username:string:Keycloak username (login)",
			FormSubmittedBy+".name:string:Display name (login)",
			FormSubmittedBy+".sub:string:Keycloak user id (login)",
			"test:boolean:Set when run from the editor",
		)},
	}
}

func (formTrigger) Validate(n *workflow.Node) error {
	_, err := ParseForm(n.Params)
	return err
}

func (formTrigger) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	if len(ec.Items) > 0 {
		return passthrough(ec)
	}
	// Manual run from the editor: one sample answer per question.
	f, err := ParseForm(ec.Node.Params)
	if err != nil {
		return nil, err
	}
	item := Item{FormSubmittedAt: time.Now().UTC().Format(time.RFC3339), "test": true}
	switch f.Protection {
	case FormProtectLogin:
		item[FormSubmittedBy] = map[string]any{"sub": "test-user", "username": "test", "name": "Test User", "email": "test@example.com"}
	case FormProtectEmail:
		item[FormSubmittedBy] = map[string]any{"email": "test@example.com"}
	}
	for _, fl := range f.Fields {
		switch fl.Type {
		case FormChoice:
			item[fl.Key] = fl.Options[0]
		case FormMultiChoice:
			item[fl.Key] = []any{fl.Options[0]}
		default:
			item[fl.Key] = "Sample answer"
		}
	}
	return Output{"main": {item}}, nil
}

// FormField is one question of a form.
type FormField struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Options     []string `json:"options,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
}

// Form is the parsed configuration of a form trigger.
type Form struct {
	Title          string
	Description    string
	Fields         []FormField
	SubmitLabel    string
	SuccessMessage string
	Protection     string
	// EmailDomains restricts access codes to these domains (empty: any).
	EmailDomains []string
}

// ParseForm validates and parses the parameters of a form trigger.
func ParseForm(params map[string]any) (*Form, error) {
	str := func(k, def string) string {
		if s, _ := params[k].(string); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		return def
	}
	f := &Form{
		Title:          str("title", "Form"),
		Description:    str("description", ""),
		SubmitLabel:    str("submitLabel", "Submit"),
		SuccessMessage: str("successMessage", "Thank you! Your response has been recorded."),
		Protection:     str("protection", FormProtectNone),
	}
	switch f.Protection {
	case FormProtectNone, FormProtectCaptcha, FormProtectLogin, FormProtectEmail:
	default:
		return nil, fmt.Errorf("invalid protection %q", f.Protection)
	}
	for _, d := range strings.FieldsFunc(str("emailDomains", ""), func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '\n' }) {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
		if d == "" {
			continue
		}
		if !strings.Contains(d, ".") || strings.ContainsAny(d, "@/") {
			return nil, fmt.Errorf("invalid e-mail domain %q", d)
		}
		f.EmailDomains = append(f.EmailDomains, d)
	}
	raw, ok := params["fields"]
	if !ok || raw == nil {
		raw = defaultFormFields()
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errors.New("questions must be a list")
	}
	if len(list) == 0 {
		return nil, errors.New("add at least one question")
	}
	if len(list) > maxFormFields {
		return nil, fmt.Errorf("at most %d questions are allowed", maxFormFields)
	}
	seen := map[string]bool{}
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("question %d: expected an object", i+1)
		}
		s := func(k string) string { v, _ := m[k].(string); return strings.TrimSpace(v) }
		fl := FormField{
			Key: s("key"), Label: s("label"), Type: s("type"),
			Placeholder: s("placeholder"), Help: s("help"),
		}
		fl.Required, _ = m["required"].(bool)
		if fl.Label == "" {
			return nil, fmt.Errorf("question %d: a label is required", i+1)
		}
		if fl.Key == "" {
			fl.Key = FormKey(fl.Label)
		}
		name := fmt.Sprintf("question %d (%s)", i+1, fl.Label)
		if !formKeyRe.MatchString(fl.Key) {
			return nil, fmt.Errorf("%s: key %q may only contain letters, digits and _ and must not start with a digit", name, fl.Key)
		}
		if fl.Key == FormSubmittedAt || fl.Key == FormSubmittedBy || fl.Key == "test" {
			return nil, fmt.Errorf("%s: key %q is reserved", name, fl.Key)
		}
		if seen[fl.Key] {
			return nil, fmt.Errorf("%s: duplicate key %q", name, fl.Key)
		}
		seen[fl.Key] = true
		if fl.Type == "" {
			fl.Type = FormText
		}
		switch fl.Type {
		case FormText, FormTextarea:
		case FormChoice, FormMultiChoice:
			fl.Options = formOptions(m["options"])
			if len(fl.Options) == 0 {
				return nil, fmt.Errorf("%s: add at least one option", name)
			}
			if len(fl.Options) > maxFormOptions {
				return nil, fmt.Errorf("%s: at most %d options are allowed", name, maxFormOptions)
			}
		default:
			return nil, fmt.Errorf("%s: unknown type %q", name, fl.Type)
		}
		f.Fields = append(f.Fields, fl)
	}
	return f, nil
}

// formOptions reads options from a list or a newline separated string;
// blanks and duplicates are dropped.
func formOptions(v any) []string {
	var raw []string
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				raw = append(raw, s)
			} else if e != nil {
				raw = append(raw, fmt.Sprint(e))
			}
		}
	case []string:
		raw = t
	case string:
		raw = strings.Split(t, "\n")
	}
	var out []string
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

var formKeyTranslit = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss", "é", "e", "è", "e", "à", "a", "ç", "c")

// FormKey derives a field key from a label: "E-mail address" → "e_mail_address".
func FormKey(label string) string {
	var b strings.Builder
	sep := false
	label = formKeyTranslit.Replace(strings.ToLower(label))
	for _, r := range label {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if sep && b.Len() > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			sep = false
		} else {
			sep = true
		}
	}
	k := b.String()
	if k == "" {
		return "field"
	}
	if k[0] >= '0' && k[0] <= '9' {
		k = "q_" + k
	}
	return k
}

// Submit checks submitted values (as from url.Values) against the form. It
// returns the output item, or per-field error messages keyed by field key.
func (f *Form) Submit(values map[string][]string) (Item, map[string]string) {
	item := Item{}
	errs := map[string]string{}
	for _, fl := range f.Fields {
		vals := values[fl.Key]
		switch fl.Type {
		case FormText, FormTextarea:
			v := ""
			if len(vals) > 0 {
				v = strings.TrimSpace(vals[0])
			}
			limit := maxFormTextLen
			if fl.Type == FormTextarea {
				limit = maxFormTextareaLn
				v = strings.ReplaceAll(v, "\r\n", "\n")
			} else {
				v = strings.Join(strings.Fields(v), " ")
			}
			switch {
			case !utf8.ValidString(v):
				errs[fl.Key] = "Invalid characters."
			case utf8.RuneCountInString(v) > limit:
				errs[fl.Key] = fmt.Sprintf("Please use at most %d characters.", limit)
			case fl.Required && v == "":
				errs[fl.Key] = "This question is required."
			}
			item[fl.Key] = v
		case FormChoice:
			v := ""
			if len(vals) > 0 {
				v = vals[0]
			}
			switch {
			case v == "" && fl.Required:
				errs[fl.Key] = "Please choose an option."
			case v != "" && !slices.Contains(fl.Options, v):
				errs[fl.Key] = "Please choose one of the options."
			}
			item[fl.Key] = v
		case FormMultiChoice:
			picked := []any{}
			for _, o := range fl.Options { // keep the configured order
				if slices.Contains(vals, o) {
					picked = append(picked, o)
				}
			}
			for _, v := range vals {
				if !slices.Contains(fl.Options, v) {
					errs[fl.Key] = "Please choose from the options."
				}
			}
			if fl.Required && len(picked) == 0 && errs[fl.Key] == "" {
				errs[fl.Key] = "Please choose at least one option."
			}
			item[fl.Key] = picked
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	item[FormSubmittedAt] = time.Now().UTC().Format(time.RFC3339)
	return item, nil
}

// EmailAllowed reports whether an access code may be sent to the address
// (already lower-case) under the form's domain restriction.
func (f *Form) EmailAllowed(addr string) bool {
	if len(f.EmailDomains) == 0 {
		return true
	}
	_, domain, ok := strings.Cut(addr, "@")
	if !ok {
		return false
	}
	for _, d := range f.EmailDomains {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}
