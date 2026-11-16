package push

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// reconcilerLease: one replica at a time talks to the providers.
const reconcilerLease = "push-reconciler"

const (
	// renewMargin: a subscription is renewed at the latest this long before it expires.
	renewMargin = time.Hour
	maxBackoff  = time.Hour
	// maxUnsubscribeFailures: an unwanted subscription that cannot be removed
	// is forgotten after this many attempts (it expires at the provider).
	maxUnsubscribeFailures = 5
)

// subKey identifies the subscription of a trigger (or of its account).
func subKey(module string, spec *nodes.PushSpec, workflowID, nodeID, connectionID string) string {
	if spec.PerConnection() {
		return module + ":" + connectionID
	}
	return module + ":" + workflowID + "/" + nodeID
}

// renewAt is when a subscription is renewed next; without a last renewal
// (see store.PushRenewNow) right away.
func renewAt(p *store.PushSubscription, spec *nodes.PushSpec) time.Time {
	if p.RenewedAt == nil {
		return time.Time{}
	}
	at := p.RenewedAt.Add(spec.RenewInterval())
	if p.ExpiresAt != nil && p.ExpiresAt.Add(-renewMargin).Before(at) {
		at = p.ExpiresAt.Add(-renewMargin)
	}
	return at
}

func backoff(failures int) time.Duration {
	d := time.Minute << min(failures-1, 10)
	return min(d, maxBackoff)
}

func (s *Service) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(s.opts.ReconcileEvery)
	defer t.Stop()
	held := false
	for {
		got, err := s.st.AcquireLease(ctx, reconcilerLease, s.opts.Instance, 3*s.opts.ReconcileEvery)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				s.log.Error("push: reconciler lease", "err", err)
			}
		case got:
			if !held {
				s.log.Info("push: this replica reconciles subscriptions")
			}
			if err := s.reconcile(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("push: reconcile", "err", err)
			}
		}
		held = got
		select {
		case <-ctx.Done():
			if held {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.st.ReleaseLease(rctx, reconcilerLease, s.opts.Instance)
				cancel()
			}
			return
		case <-t.C:
		case <-s.pokeRe:
		}
	}
}

// want is a subscription the active triggers need.
type want struct {
	key         string
	org, module string
	endpoint    string
	conn        string
	nodeType    string
	spec        *nodes.PushSpec
	pusher      nodes.Pusher
	def         nodes.Definition
	node        *workflow.Node // the first trigger's node (its params feed the templates)
	triggers    []store.TriggerRef
	url         string
	fingerprint string
}

// desiredSubscriptions computes the subscriptions of the active Gmail and
// Graph triggers. Nothing is wanted while push is off or the public URL
// cannot be reached by providers, so existing subscriptions are removed.
func (s *Service) desiredSubscriptions(ctx context.Context) (map[string]*want, error) {
	out := map[string]*want{}
	if !s.opts.Enabled || !s.publicHTTPS() {
		return out, nil
	}
	wfs, err := s.activeWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(wfs, func(a, b int) bool { return wfs[a].ID < wfs[b].ID })
	configs := map[string]map[string]any{}
	for _, t := range s.pushTriggers(wfs, nodes.PushGmail, nodes.PushGraph) {
		pusher, ok := t.impl.(nodes.Pusher)
		if !ok {
			continue
		}
		spec := pusher.PushSpec()
		conn, _ := t.node.Params[nodes.ConnectionParam].(string)
		if conn == "" {
			continue
		}
		ck := t.wf.OrgID + "\x00" + t.def.Module
		cfg, ok := configs[ck]
		if !ok {
			if cfg, _, err = s.opts.Settings.ModuleSettings(ctx, t.wf.OrgID, t.def.Module); err != nil {
				return nil, err
			}
			configs[ck] = cfg
		}
		if s.missingConfig(ctx, t.wf.OrgID, t.def.Module, spec) != "" {
			continue
		}
		key := subKey(t.def.Module, spec, t.wf.ID, t.node.ID, conn)
		w := out[key]
		if w == nil {
			w = &want{key: key, org: t.wf.OrgID, module: t.def.Module, endpoint: spec.Endpoint, conn: conn,
				nodeType: t.node.Type, spec: spec, pusher: pusher, def: t.def, node: t.node}
			switch spec.Endpoint {
			case nodes.PushGmail:
				if w.url, err = s.GmailEndpoint(ctx, t.wf.OrgID); err != nil {
					return nil, err
				}
			case nodes.PushGraph:
				w.url = s.GraphEndpoint()
			}
			// What the subscription is made of: a change replaces it.
			parts := []any{spec.Endpoint, t.node.Type, conn, w.url}
			for _, r := range spec.Requires {
				parts = append(parts, cfg[r])
			}
			if !spec.PerConnection() {
				params := map[string]any{}
				for k, v := range t.node.Params {
					if k != nodes.PollParam && k != nodes.DeliveryParam {
						params[k] = v
					}
				}
				parts = append(parts, params)
			}
			b, _ := json.Marshal(parts)
			sum := sha256.Sum256(b)
			w.fingerprint = hex.EncodeToString(sum[:])
			out[key] = w
		}
		w.triggers = append(w.triggers, t.ref())
	}
	return out, nil
}

// reconcile converges the stored subscriptions with the wanted ones: it
// subscribes, renews when due and unsubscribes. Failures back off per
// subscription (1 min doubling up to 1 h), so providers are not called on
// every pass.
func (s *Service) reconcile(ctx context.Context) error {
	wanted, err := s.desiredSubscriptions(ctx)
	if err != nil {
		return err
	}
	actual, err := s.st.ListPushSubscriptions(ctx)
	if err != nil {
		return err
	}
	have := map[string]*store.PushSubscription{}
	for _, p := range actual {
		have[p.Key] = p
	}
	keys := make([]string, 0, len(wanted))
	for k := range wanted {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := s.converge(ctx, wanted[k], have[k]); err != nil {
			return err
		}
	}
	for _, p := range actual {
		if wanted[p.Key] == nil {
			if err := s.remove(ctx, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// stillLeader renews the reconciler lease before a provider call, so a pass
// that runs long never overlaps with another replica's.
func (s *Service) stillLeader(ctx context.Context) error {
	got, err := s.st.AcquireLease(ctx, reconcilerLease, s.opts.Instance, 3*s.opts.ReconcileEvery)
	if err == nil && !got {
		err = errors.New("lost the reconciler lease")
	}
	return err
}

func (s *Service) converge(ctx context.Context, w *want, p *store.PushSubscription) error {
	now := time.Now()
	if p == nil {
		p = &store.PushSubscription{Key: w.key}
	}
	changed := p.OrgID != w.org || p.Module != w.module || p.Endpoint != w.endpoint || p.ConnectionID != w.conn ||
		p.NodeType != w.nodeType || !sameTriggers(p.Triggers, w.triggers) || p.CreatedAt.IsZero()
	p.OrgID, p.Module, p.Endpoint, p.ConnectionID, p.NodeType, p.Triggers = w.org, w.module, w.endpoint, w.conn, w.nodeType, w.triggers
	save := func() error { return s.st.SavePushSubscription(ctx, p) }
	if p.RetryAt != nil && now.Before(*p.RetryAt) {
		if changed {
			return save()
		}
		return nil
	}
	var action string
	switch {
	case p.ExternalID == "" || p.Fingerprint != w.fingerprint:
		action = "subscribe"
	case p.ExpiresAt == nil || !now.Before(renewAt(p, w.spec)):
		action = "renew"
	default:
		if changed {
			return save()
		}
		return nil
	}
	if err := s.stillLeader(ctx); err != nil {
		return err
	}
	ec, err := s.pushContext(ctx, w.org, w.module, w.def, w.node, w.conn)
	if err == nil {
		err = s.call(ctx, ec, w, p, action)
	}
	if err != nil {
		p.Failures++
		retry := now.Add(backoff(p.Failures))
		p.RetryAt, p.LastError = &retry, err.Error()
		s.log.Warn("push: "+action+" failed", "subscription", p.Key, "err", err, "retryAt", retry)
	} else {
		p.Failures, p.RetryAt, p.LastError = 0, nil, ""
	}
	return save()
}

// call subscribes or renews. A renewal the provider no longer knows becomes
// a new subscription.
func (s *Service) call(ctx context.Context, ec *nodes.ExecContext, w *want, p *store.PushSubscription, action string) error {
	vars := nodes.PushVars{NotificationURL: w.url, ID: p.ExternalID}
	if l := w.spec.LifetimeDuration(); l > 0 {
		vars.Expires = time.Now().Add(l)
	}
	if action == "renew" {
		if p.Secret != "" {
			var err error
			if vars.ClientState, err = s.opts.Box.Open(p.Secret, secretAAD(p.Key)); err != nil {
				return err
			}
		}
		res, err := w.pusher.PushCall(ctx, ec, "renew", vars)
		var he *nodes.PushHTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 410) {
			s.log.Info("push: subscription gone at the provider; subscribing again", "subscription", p.Key)
			p.ExternalID = ""
			return s.call(ctx, ec, w, p, "subscribe")
		}
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		p.ExpiresAt, p.RenewedAt = &res.Expires, &now
		if res.ID != "" {
			p.ExternalID = res.ID
		}
		return nil
	}
	if p.ExternalID != "" { // replaced: remove the old one first (best effort)
		old := vars
		old.ID = p.ExternalID
		if _, err := w.pusher.PushCall(ctx, ec, "unsubscribe", old); err != nil {
			s.log.Warn("push: remove replaced subscription", "subscription", p.Key, "err", err)
		}
		p.ExternalID = ""
	}
	state := randomSecret()
	sealed, err := s.opts.Box.Seal(state, secretAAD(p.Key))
	if err != nil {
		return err
	}
	vars.ID, vars.ClientState = "", state
	res, err := w.pusher.PushCall(ctx, ec, "subscribe", vars)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	p.ExternalID, p.Secret, p.Fingerprint = res.ID, sealed, w.fingerprint
	if p.ExternalID == "" {
		p.ExternalID = p.Key // providers without ids (Gmail): one watch per account
	}
	p.ExpiresAt, p.RenewedAt = &res.Expires, &now
	s.log.Info("push: subscribed", "subscription", p.Key, "expires", res.Expires)
	return nil
}

// remove unsubscribes a subscription no trigger needs and forgets it.
func (s *Service) remove(ctx context.Context, p *store.PushSubscription) error {
	now := time.Now()
	if p.RetryAt != nil && now.Before(*p.RetryAt) {
		return nil
	}
	impl, ok := s.opts.Registry.Get(p.NodeType)
	pusher, isPusher := impl.(nodes.Pusher)
	if !ok || !isPusher || p.ExternalID == "" || (p.ExpiresAt != nil && p.ExpiresAt.Before(now)) {
		return s.st.DeletePushSubscription(ctx, p.Key)
	}
	if err := s.stillLeader(ctx); err != nil {
		return err
	}
	def := impl.Definition()
	ec, err := s.pushContext(ctx, p.OrgID, p.Module, def, &workflow.Node{Type: p.NodeType, Params: map[string]any{}}, p.ConnectionID)
	if err == nil {
		vars := nodes.PushVars{ID: p.ExternalID}
		_, err = pusher.PushCall(ctx, ec, "unsubscribe", vars)
	}
	var he *nodes.PushHTTPError
	if err == nil || (errors.As(err, &he) && (he.Status == 404 || he.Status == 410)) || p.Failures+1 >= maxUnsubscribeFailures {
		if err != nil {
			s.log.Warn("push: forgetting subscription that could not be removed", "subscription", p.Key, "err", err)
		} else {
			s.log.Info("push: unsubscribed", "subscription", p.Key)
		}
		return s.st.DeletePushSubscription(ctx, p.Key)
	}
	p.Failures++
	retry := now.Add(backoff(p.Failures))
	p.RetryAt, p.LastError = &retry, err.Error()
	return s.st.SavePushSubscription(ctx, p)
}

// pushContext is the context of a provider call: the account's fresh token
// (through the OAuth manager) and the module settings.
func (s *Service) pushContext(ctx context.Context, org, module string, def nodes.Definition, n *workflow.Node, conn string) (*nodes.ExecContext, error) {
	tok, err := s.opts.Tokens.Token(ctx, org, module, conn)
	if err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}
	cfg, vars, err := s.opts.Settings.ModuleSettings(ctx, org, module)
	if err != nil {
		return nil, err
	}
	return &nodes.ExecContext{
		Node: n, Def: &def, Items: []nodes.Item{}, Config: cfg, Vars: vars, OAuth: tok,
		Env: expression.WithContext(expression.Env{}, ctx), Logger: s.log,
	}, nil
}

func sameTriggers(a, b []store.TriggerRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func secretAAD(key string) string { return "push/" + key }

func randomSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
