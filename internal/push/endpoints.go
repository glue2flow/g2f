package push

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
)

const (
	maxNotificationBytes = 1 << 20
	maxValidationToken   = 4096
)

// Handler serves the public push endpoints (mounted at /push/):
//
//	POST /push/gmail/{token}  Google Cloud Pub/Sub push of Gmail watches
//	POST /push/graph          Microsoft Graph change and lifecycle notifications
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /push/gmail/{token}", s.gmail)
	mux.HandleFunc("POST /push/graph", s.graph)
	return mux
}

// wakeAll wakes the triggers of a subscription. Failures are logged: the
// provider must not retry because of them, the safety-net poll catches up.
func (s *Service) wakeAll(ctx context.Context, p *store.PushSubscription) {
	for _, t := range p.Triggers {
		if err := s.opts.Waker.Wake(ctx, t.WorkflowID, t.NodeID); err != nil {
			s.log.Error("push: wake trigger", "workflow", t.WorkflowID, "node", t.NodeID, "err", err)
		}
	}
	if err := s.st.PushNotified(ctx, p.Key); err != nil {
		s.log.Error("push: record notification", "err", err)
	}
}

// gmail handles a Pub/Sub push message. The secret token in the path names
// the organization (unknown tokens: 404). If the Gmail settings name the
// push service account, the OIDC token Pub/Sub attaches must be valid too
// (401 otherwise). Well-formed messages are acknowledged with 204 whether
// or not the address belongs to a trigger.
func (s *Service) gmail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := r.PathValue("token")
	org, err := s.st.OrgForPushToken(ctx, nodes.PushGmail, tokenHash(token))
	if errors.Is(err, store.ErrNotFound) || token == "" {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("push: gmail token", "err", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := s.verifyPubSub(ctx, r, org, token); err != nil {
		s.log.Warn("push: gmail notification rejected", "org", org, "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var env struct {
		Message struct {
			Data string `json:"data"`
		} `json:"message"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxNotificationBytes))
	if err != nil || json.Unmarshal(body, &env) != nil || env.Message.Data == "" {
		http.Error(w, "malformed Pub/Sub message", http.StatusBadRequest)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(env.Message.Data)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(env.Message.Data)
	}
	var note struct {
		EmailAddress string          `json:"emailAddress"`
		HistoryID    json.RawMessage `json:"historyId"`
	}
	if err != nil || json.Unmarshal(raw, &note) != nil || note.EmailAddress == "" {
		http.Error(w, "malformed Gmail notification", http.StatusBadRequest)
		return
	}
	subs, err := s.st.PushSubscriptionsForAccount(ctx, org, nodes.PushGmail, note.EmailAddress)
	if err != nil {
		s.log.Error("push: gmail lookup", "err", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable) // Pub/Sub retries
		return
	}
	for _, p := range subs {
		s.wakeAll(context.WithoutCancel(ctx), p)
	}
	w.WriteHeader(http.StatusNoContent)
}

// verifyPubSub checks the OIDC token of a push request when the
// organization's Gmail settings name the expected service account.
func (s *Service) verifyPubSub(ctx context.Context, r *http.Request, org, token string) error {
	cfg, _, err := s.opts.Settings.ModuleSettings(ctx, org, gmailModule(s))
	if err != nil {
		return err
	}
	account, _ := cfg["pushServiceAccount"].(string)
	if account = strings.TrimSpace(account); account == "" {
		return nil // the secret path alone authenticates
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return errors.New("no OIDC token (enable authentication on the push subscription)")
	}
	keys := s.opts.GoogleKeys
	audience := s.opts.PublicURL + "/push/gmail/" + token
	idt, err := oidc.NewVerifier(googleIssuer, keys, &oidc.Config{ClientID: audience}).Verify(ctx, raw)
	if err != nil {
		return err
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err := idt.Claims(&claims); err != nil {
		return err
	}
	if !claims.Verified || !strings.EqualFold(claims.Email, account) {
		return errors.New("token of another service account: " + claims.Email)
	}
	return nil
}

// gmailModule is the module whose settings configure Gmail push (the first
// module with a Gmail push trigger, normally "gmail").
func gmailModule(s *Service) string {
	for _, d := range s.opts.Registry.Definitions() {
		if d.Push == nodes.PushGmail {
			return d.Module
		}
	}
	return "gmail"
}

// graph answers the validation handshake (echo validationToken as text
// within 10 s) and handles notifications: each must carry the clientState
// of its subscription (compared in constant time); others are dropped.
// Lifecycle notifications renew (reauthorizationRequired), recreate
// (subscriptionRemoved) or check (missed). The answer is 202 for any
// well-formed body, so it reveals nothing about subscriptions.
func (s *Service) graph(w http.ResponseWriter, r *http.Request) {
	if v := r.URL.Query().Get("validationToken"); v != "" {
		if len(v) > maxValidationToken {
			http.Error(w, "validation token too long", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, v)
		return
	}
	var body struct {
		Value []struct {
			SubscriptionID string `json:"subscriptionId"`
			ClientState    string `json:"clientState"`
			LifecycleEvent string `json:"lifecycleEvent"`
		} `json:"value"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxNotificationBytes))
	if err != nil || json.Unmarshal(raw, &body) != nil {
		http.Error(w, "malformed notification", http.StatusBadRequest)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	woken := map[string]bool{}
	for _, n := range body.Value {
		if n.SubscriptionID == "" {
			continue
		}
		p, err := s.st.PushSubscriptionByExternalID(ctx, nodes.PushGraph, n.SubscriptionID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Error("push: graph lookup", "err", err)
			}
			continue
		}
		state, err := s.opts.Box.Open(p.Secret, secretAAD(p.Key))
		if err != nil || subtle.ConstantTimeCompare([]byte(state), []byte(n.ClientState)) != 1 {
			s.log.Warn("push: graph notification with a wrong clientState dropped", "subscription", p.Key)
			continue
		}
		switch n.LifecycleEvent {
		case "reauthorizationRequired":
			if err := s.st.PushRenewNow(ctx, p.Key, false); err != nil {
				s.log.Error("push: schedule renewal", "err", err)
			}
			s.pokeAll()
			continue
		case "subscriptionRemoved":
			if err := s.st.PushRenewNow(ctx, p.Key, true); err != nil {
				s.log.Error("push: schedule resubscribe", "err", err)
			}
			s.pokeAll()
		}
		// Changes, "missed" and a removed subscription (mail may have been
		// missed meanwhile): check now, once per subscription and request.
		if !woken[p.Key] {
			woken[p.Key] = true
			s.wakeAll(ctx, p)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
