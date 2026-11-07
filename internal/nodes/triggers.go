package nodes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/glue2flow/g2f/internal/workflow"
)

// --- Schedule trigger -------------------------------------------------------

type scheduleTrigger struct{}

func (scheduleTrigger) Definition() Definition {
	return Definition{
		Type: "trigger.schedule", Name: "Schedule Trigger", Category: "Triggers", Source: "builtin", Module: BuiltinModule,
		Description: "Run on a cron schedule or once at a specific time. Only fires while the workflow is active.",
		Color:       "#10b981", Trigger: TriggerSchedule, Inputs: []string{}, Outputs: []string{"main"},
		Params: []Param{
			{Name: "mode", Label: "Mode", Type: ParamSelect, Default: "cron", NoExpression: true, Options: []Option{
				{"Recurring (cron)", "cron"}, {"Once", "once"},
			}},
			{Name: "cron", Label: "Cron expression", Type: ParamString, Default: "0 * * * *", NoExpression: true,
				ShowIf:      map[string][]string{"mode": {"cron"}},
				Placeholder: "*/15 * * * *",
				Description: "minute hour day-of-month month day-of-week, or @hourly, @daily, @weekly, @every 30m."},
			{Name: "at", Label: "Run at (local time)", Type: ParamDateTime, NoExpression: true,
				ShowIf:      map[string][]string{"mode": {"once"}},
				Placeholder: "2026-12-24T18:00", Description: "Used in 'Once' mode. Format YYYY-MM-DDTHH:MM."},
			{Name: "timezone", Label: "Timezone", Type: ParamString, Default: "UTC", NoExpression: true,
				Placeholder: "Europe/Berlin"},
		},
	}
}

func (scheduleTrigger) Validate(n *workflow.Node) error {
	_, err := ParseSchedule(n.Params)
	return err
}

func (scheduleTrigger) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	if len(ec.Items) > 0 {
		return passthrough(ec)
	}
	// Manual run from the editor.
	now := time.Now().UTC().Format(time.RFC3339)
	return Output{"main": {{"firedAt": now, "scheduledAt": now, "manual": true}}}, nil
}

// ScheduleMode is "cron" or "once".
type ScheduleMode string

const (
	ScheduleCron ScheduleMode = "cron"
	ScheduleOnce ScheduleMode = "once"
)

// MinCronInterval prevents schedules that would flood the engine.
const MinCronInterval = time.Minute

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Schedule is a parsed schedule trigger configuration.
type Schedule struct {
	Mode     ScheduleMode
	Spec     string // cron expression (cron mode)
	At       time.Time
	Location *time.Location
	cron     cron.Schedule
}

// ParseSchedule validates and parses the parameters of a schedule trigger.
func ParseSchedule(params map[string]any) (*Schedule, error) {
	str := func(k string) string { s, _ := params[k].(string); return strings.TrimSpace(s) }
	s := &Schedule{Mode: ScheduleMode(str("mode"))}
	if s.Mode == "" {
		s.Mode = ScheduleCron
	}
	tz := str("timezone")
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q", tz)
	}
	s.Location = loc

	switch s.Mode {
	case ScheduleCron:
		s.Spec = str("cron")
		if s.Spec == "" {
			s.Spec = "0 * * * *"
		}
		if strings.HasPrefix(s.Spec, "CRON_TZ=") || strings.HasPrefix(s.Spec, "TZ=") {
			return nil, errors.New("set the timezone in the Timezone field, not in the expression")
		}
		sched, err := cronParser.Parse(s.Spec)
		if err != nil {
			return nil, fmt.Errorf("invalid cron expression: %w", err)
		}
		s.cron = sched
		// Reject sub-minute schedules (e.g. @every 10s).
		t1 := sched.Next(time.Date(2030, 1, 1, 0, 0, 0, 0, loc))
		if t2 := sched.Next(t1); t2.Sub(t1) < MinCronInterval {
			return nil, fmt.Errorf("schedule fires more often than every %s", MinCronInterval)
		}
	case ScheduleOnce:
		at := str("at")
		if at == "" {
			return nil, errors.New("'Run at' is required in 'Once' mode")
		}
		var t time.Time
		for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04"} {
			if t, err = time.ParseInLocation(layout, at, loc); err == nil {
				break
			}
		}
		if err != nil {
			if t, err = time.Parse(time.RFC3339, at); err != nil {
				return nil, fmt.Errorf("invalid time %q, use YYYY-MM-DDTHH:MM", at)
			}
		}
		s.At = t
	default:
		return nil, fmt.Errorf("invalid mode %q", s.Mode)
	}
	return s, nil
}

// Next returns the next fire time strictly after t (zero if none).
func (s *Schedule) Next(t time.Time) time.Time {
	if s.Mode == ScheduleOnce {
		if s.At.After(t) {
			return s.At
		}
		return time.Time{}
	}
	return s.cron.Next(t.In(s.Location))
}

// NextN returns up to n upcoming fire times after t.
func (s *Schedule) NextN(t time.Time, n int) []time.Time {
	var out []time.Time
	for range n {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}

// --- Webhook trigger --------------------------------------------------------

type webhookTrigger struct{}

func (webhookTrigger) Definition() Definition {
	return Definition{
		Type: "trigger.webhook", Name: "Webhook Trigger", Category: "Triggers", Source: "builtin", Module: BuiltinModule,
		Description: "Start the workflow with an HTTP request to its webhook URL. Only active workflows accept calls.",
		Color:       "#f59e0b", Trigger: TriggerWebhook, Inputs: []string{}, Outputs: []string{"main"},
		Params: []Param{
			{Name: "method", Label: "HTTP method", Type: ParamSelect, Default: "POST", NoExpression: true, Options: []Option{
				{"POST", "POST"}, {"GET", "GET"}, {"PUT", "PUT"}, {"PATCH", "PATCH"}, {"DELETE", "DELETE"}, {"Any", "ANY"},
			}},
			{Name: "auth", Label: "Authentication", Type: ParamSelect, Default: "none", NoExpression: true, Options: []Option{
				{"None", "none"},
				{"Shared token in header", "token"},
				{"HMAC-SHA256 signature of body", "hmac"},
			}},
			{Name: "secret", Label: "Secret", Type: ParamSecret, NoExpression: true,
				ShowIf:      map[string][]string{"auth": {"token", "hmac"}},
				Description: "Token value, or HMAC key."},
			{Name: "header", Label: "Header name", Type: ParamString, NoExpression: true,
				ShowIf:      map[string][]string{"auth": {"token", "hmac"}},
				Placeholder: "X-G2F-Token / X-Signature-256",
				Description: "Defaults to X-G2F-Token (token) or X-Signature-256 (hmac, value 'sha256=<hex>'). Use X-Hub-Signature-256 for GitHub."},
			{Name: "respond", Label: "Respond", Type: ParamSelect, Default: "immediately", NoExpression: true, Options: []Option{
				{"Immediately (202 + execution id)", "immediately"},
				{"With output of last node (waits)", "lastNode"},
			}},
			{Name: "testBody", Label: "Test body", Type: ParamJSON, NoExpression: true, Default: map[string]any{},
				Description: "Body used when the workflow is run manually from the editor."},
		},
	}
}

func (webhookTrigger) Validate(n *workflow.Node) error {
	_, err := ParseWebhook(n.Params)
	return err
}

func (webhookTrigger) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	if len(ec.Items) > 0 {
		return passthrough(ec)
	}
	body, err := toMap(ec.Raw("testBody"))
	if err != nil {
		return nil, fmt.Errorf("test body: %w", err)
	}
	method, _ := ec.Raw("method").(string)
	return Output{"main": {{
		"method": method, "headers": map[string]any{}, "query": map[string]any{},
		"body": body, "receivedAt": time.Now().UTC().Format(time.RFC3339), "test": true,
	}}}, nil
}

// Webhook is the parsed configuration of a webhook trigger.
type Webhook struct {
	Method string // POST, GET, ... or ANY
	Auth   string // none | token | hmac
	Secret string
	// SecretStored is set when params carry nodes.SecretRef instead of the value
	// (validation before the encrypted value is loaded).
	SecretStored bool
	Header       string
	Respond      string // immediately | lastNode
}

func ParseWebhook(params map[string]any) (*Webhook, error) {
	str := func(k, def string) string {
		if s, _ := params[k].(string); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		return def
	}
	w := &Webhook{
		Method:  strings.ToUpper(str("method", "POST")),
		Auth:    str("auth", "none"),
		Secret:  str("secret", ""),
		Respond: str("respond", "immediately"),
	}
	switch w.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "ANY":
	default:
		return nil, fmt.Errorf("invalid method %q", w.Method)
	}
	switch w.Auth {
	case "none":
	case "token":
		w.Header = str("header", "X-G2F-Token")
	case "hmac":
		w.Header = str("header", "X-Signature-256")
	default:
		return nil, fmt.Errorf("invalid authentication %q", w.Auth)
	}
	if IsSecretRef(params["secret"]) {
		w.SecretStored = true
	}
	if w.Auth != "none" && w.Secret == "" && !w.SecretStored {
		return nil, errors.New("a secret is required when authentication is enabled")
	}
	if w.Respond != "immediately" && w.Respond != "lastNode" {
		return nil, fmt.Errorf("invalid respond mode %q", w.Respond)
	}
	return w, nil
}
