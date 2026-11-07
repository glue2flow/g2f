package trigger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wolfposd/g8g/internal/engine"
	"github.com/wolfposd/g8g/internal/nodes"
	"github.com/wolfposd/g8g/internal/store"
)

const maxWebhookBody = 5 << 20 // 5 MiB

// WebhookHandler serves POST/GET/... /webhook/{workflowId}/{nodeId}.
type WebhookHandler struct {
	store Store
	eng   Engine
	reg   *nodes.Registry
	log   *slog.Logger
	// secrets provides the node's encrypted parameters (the webhook secret).
	secrets engine.SecretSource
	// MaxWait bounds "respond with last node" requests.
	maxWait time.Duration
}

func NewWebhookHandler(st Store, eng Engine, reg *nodes.Registry, sec engine.SecretSource, log *slog.Logger, maxWait time.Duration) *WebhookHandler {
	if maxWait <= 0 {
		maxWait = time.Minute
	}
	return &WebhookHandler{store: st, eng: eng, reg: reg, secrets: sec, log: log.With("component", "webhook"), maxWait: maxWait}
}

// URL returns the public URL of a webhook trigger.
func URL(publicURL, workflowID, nodeID string) string {
	return strings.TrimRight(publicURL, "/") + "/webhook/" + workflowID + "/" + url.PathEscape(nodeID)
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/webhook/"), "/"), "/")
	if len(parts) != 2 {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	wfID, nodeID := parts[0], parts[1]
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	wf, err := h.store.GetWorkflow(r.Context(), wfID)
	if errors.Is(err, store.ErrNotFound) {
		jsonError(w, http.StatusNotFound, "webhook not found")
		return
	} else if err != nil {
		h.log.Error("load workflow", "err", err)
		jsonError(w, http.StatusInternalServerError, "internal error")
		return
	}
	node := wf.Graph.Node(nodeID)
	if node == nil || node.Disabled || h.reg.TriggerKind(node.Type) != nodes.TriggerWebhook {
		jsonError(w, http.StatusNotFound, "webhook not found")
		return
	}
	if !wf.Active {
		jsonError(w, http.StatusNotFound, "workflow is not active")
		return
	}
	params := node.Params
	if h.secrets != nil {
		sec, err := h.secrets.NodeSecrets(r.Context(), wf.ID)
		if err != nil {
			h.log.Error("load webhook secret", "err", err)
			jsonError(w, http.StatusInternalServerError, "internal error")
			return
		}
		params = maps.Clone(node.Params)
		for k, v := range sec[node.ID] {
			params[k] = v
		}
	}
	cfg, err := nodes.ParseWebhook(params)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "webhook misconfigured: "+err.Error())
		return
	}
	if cfg.Method != "ANY" && r.Method != cfg.Method {
		w.Header().Set("Allow", cfg.Method)
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed, use "+cfg.Method)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		jsonError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	if !verify(cfg, r, body) {
		jsonError(w, http.StatusUnauthorized, "invalid or missing credentials")
		return
	}

	req := engine.Request{
		Workflow:    wf,
		TriggerType: nodes.TriggerWebhook,
		TriggerNode: nodeID,
		TriggeredBy: "webhook",
		Items:       []nodes.Item{requestItem(r, body, cfg)},
	}

	if cfg.Respond != "lastNode" {
		exec, err := h.eng.Start(r.Context(), req)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("X-G8G-Execution-Id", exec.ID)
		writeJSON(w, http.StatusAccepted, map[string]string{"executionId": exec.ID, "status": string(exec.Status)})
		return
	}

	// Wait for the result, but do not abort the run if the caller disconnects.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), h.maxWait)
	defer cancel()
	res, err := h.eng.Run(ctx, req)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("X-G8G-Execution-Id", res.Execution.ID)
	if res.Execution.Status != store.StatusSuccess {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": res.Execution.Error, "executionId": res.Execution.ID,
		})
		return
	}
	var out any = res.LastOutput
	if len(res.LastOutput) == 1 {
		out = res.LastOutput[0]
	} else if res.LastOutput == nil {
		out = []any{}
	}
	writeJSON(w, http.StatusOK, out)
}

func verify(cfg *nodes.Webhook, r *http.Request, body []byte) bool {
	if cfg.Auth != "none" && cfg.Secret == "" {
		return false // fail closed if the secret could not be loaded
	}
	switch cfg.Auth {
	case "token":
		got := r.Header.Get(cfg.Header)
		got = strings.TrimPrefix(got, "Bearer ")
		return subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Secret)) == 1
	case "hmac":
		got := strings.TrimPrefix(r.Header.Get(cfg.Header), "sha256=")
		sig, err := hex.DecodeString(got)
		if err != nil {
			return false
		}
		mac := hmac.New(sha256.New, []byte(cfg.Secret))
		mac.Write(body)
		return hmac.Equal(sig, mac.Sum(nil))
	default:
		return true
	}
}

// requestItem converts the HTTP request into the trigger item.
func requestItem(r *http.Request, body []byte, cfg *nodes.Webhook) nodes.Item {
	headers := map[string]any{}
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if lk == "cookie" || (cfg.Header != "" && strings.EqualFold(k, cfg.Header)) {
			continue // never pass credentials into workflow data
		}
		headers[lk] = strings.Join(v, ", ")
	}
	query := map[string]any{}
	for k, v := range r.URL.Query() {
		if len(v) == 1 {
			query[k] = v[0]
		} else {
			l := make([]any, len(v))
			for i, s := range v {
				l[i] = s
			}
			query[k] = l
		}
	}
	return nodes.Item{
		"method":     r.Method,
		"headers":    headers,
		"query":      query,
		"body":       parseBody(r.Header.Get("Content-Type"), body),
		"receivedAt": time.Now().UTC().Format(time.RFC3339),
	}
}

func parseBody(contentType string, body []byte) any {
	if len(body) == 0 {
		return nil
	}
	mt, _, _ := mime.ParseMediaType(contentType)
	switch {
	case mt == "application/x-www-form-urlencoded":
		if vals, err := url.ParseQuery(string(body)); err == nil {
			m := map[string]any{}
			for k, v := range vals {
				m[k] = strings.Join(v, ",")
			}
			return m
		}
	case strings.Contains(mt, "json") || mt == "":
		var v any
		if json.Unmarshal(body, &v) == nil {
			return v
		}
	}
	return string(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
