package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/glue2flow/g2f/internal/auth"
	"github.com/glue2flow/g2f/internal/config"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/push"
	"github.com/glue2flow/g2f/internal/trigger"
	"github.com/glue2flow/g2f/internal/workflow"
	"github.com/glue2flow/g2f/modules/email"
)

type triggerInfo struct {
	NodeID string            `json:"nodeId"`
	Name   string            `json:"name"`
	Kind   nodes.TriggerKind `json:"kind"`
	// Listening is true when the trigger currently fires on its own
	// (workflow active and node enabled). Manual triggers are always runnable.
	Listening bool          `json:"listening"`
	Webhook   *webhookInfo  `json:"webhook,omitempty"`
	Form      *formInfo     `json:"form,omitempty"`
	Schedule  *scheduleInfo `json:"schedule,omitempty"`
	Poll      *pollInfo     `json:"poll,omitempty"`
	Error     string        `json:"error,omitempty"`
}

type webhookInfo struct {
	URL     string `json:"url"`
	Method  string `json:"method"`
	Auth    string `json:"auth"`
	Header  string `json:"header,omitempty"`
	Respond string `json:"respond"`
}

type formInfo struct {
	URL        string `json:"url"`
	Questions  int    `json:"questions"`
	Protection string `json:"protection"`
	// Warning explains why the protection cannot work as configured.
	Warning string `json:"warning,omitempty"`
}

type scheduleInfo struct {
	Mode     nodes.ScheduleMode `json:"mode"`
	Timezone string             `json:"timezone"`
	Next     []time.Time        `json:"next"`
}

type pollInfo struct {
	// IntervalMinutes is the current time between checks: the configured
	// interval, or at least the push safety net while push works.
	IntervalMinutes   float64        `json:"intervalMinutes"`
	ConfiguredMinutes float64        `json:"configuredMinutes"`
	Next              time.Time      `json:"next"`
	Delivery          *push.Delivery `json:"delivery,omitempty"`
}

// listTriggers describes how each trigger of the stored workflow is invoked.
func (s *Server) listTriggers(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.loadWorkflow(w, r, chi.URLParam(r, "id"), workflow.RoleViewer)
	if !ok {
		return
	}
	// Secrets are not in the graph; mark stored ones so validation sees them.
	if err := s.secrets.MarkStored(r.Context(), wf); err != nil {
		s.storeError(w, err)
		return
	}
	out := []triggerInfo{}
	for _, n := range wf.Graph.Nodes {
		kind := s.reg.TriggerKind(n.Type)
		if kind == "" {
			continue
		}
		info := triggerInfo{NodeID: n.ID, Name: n.Name, Kind: kind, Listening: wf.Active && !n.Disabled}
		switch kind {
		case nodes.TriggerManual:
			info.Listening = !n.Disabled
		case nodes.TriggerWebhook:
			cfg, err := nodes.ParseWebhook(n.Params)
			if err != nil {
				info.Error = err.Error()
				break
			}
			info.Webhook = &webhookInfo{
				URL: trigger.URL(s.cfg.PublicURL, wf.ID, n.ID), Method: cfg.Method,
				Auth: cfg.Auth, Header: cfg.Header, Respond: cfg.Respond,
			}
		case nodes.TriggerForm:
			f, err := nodes.ParseForm(n.Params)
			if err != nil {
				info.Error = err.Error()
				break
			}
			info.Form = &formInfo{URL: trigger.FormURL(s.cfg.PublicURL, wf.ID, n.ID), Questions: len(f.Fields), Protection: f.Protection}
			switch f.Protection {
			case nodes.FormProtectEmail:
				cfg, _, err := s.secrets.ModuleSettings(r.Context(), wf.OrgID, "email")
				if err != nil || !email.Configured(cfg) {
					info.Form.Warning = "E-mail is not configured: an admin must set the SMTP server in Plugins → Email → Settings. Until then the form is unavailable."
				}
			case nodes.FormProtectLogin:
				if s.cfg.Auth.Mode == config.AuthNone {
					info.Form.Warning = "Authentication is disabled on this server (G2F_AUTH_MODE=none): everyone answers as the development user."
				}
			}
		case nodes.TriggerPoll:
			d := nodes.PollInterval(n.Params)
			var dl *push.Delivery
			if s.delivery != nil {
				u := auth.FromContext(r.Context())
				dl = s.delivery.Delivery(r.Context(), wf, &n, u != nil && u.Admin)
			}
			eff := trigger.EffectiveInterval(d, dl != nil && dl.Push)
			info.Poll = &pollInfo{
				IntervalMinutes: eff.Minutes(), ConfiguredMinutes: d.Minutes(),
				Next: trigger.NextPoll(wf.ID, n.ID, eff, time.Now()), Delivery: dl,
			}
		case nodes.TriggerSchedule:
			sched, err := nodes.ParseSchedule(n.Params)
			if err != nil {
				info.Error = err.Error()
				break
			}
			info.Schedule = &scheduleInfo{
				Mode: sched.Mode, Timezone: sched.Location.String(), Next: sched.NextN(time.Now(), 5),
			}
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}
