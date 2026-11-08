package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/trigger"
	"github.com/glue2flow/g2f/internal/workflow"
)

type triggerInfo struct {
	NodeID string            `json:"nodeId"`
	Name   string            `json:"name"`
	Kind   nodes.TriggerKind `json:"kind"`
	// Listening is true when the trigger currently fires on its own
	// (workflow active and node enabled). Manual triggers are always runnable.
	Listening bool          `json:"listening"`
	Webhook   *webhookInfo  `json:"webhook,omitempty"`
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

type scheduleInfo struct {
	Mode     nodes.ScheduleMode `json:"mode"`
	Timezone string             `json:"timezone"`
	Next     []time.Time        `json:"next"`
}

type pollInfo struct {
	IntervalMinutes float64   `json:"intervalMinutes"`
	Next            time.Time `json:"next"`
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
		case nodes.TriggerPoll:
			d := nodes.PollInterval(n.Params)
			info.Poll = &pollInfo{IntervalMinutes: d.Minutes(), Next: time.Now().Truncate(d).Add(d)}
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
