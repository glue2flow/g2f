// Package push wakes polling mail triggers when their source reports new
// mail, instead of waiting for the next check:
//
//   - IMAP IDLE: one replica per trigger (a lease) keeps a connection open
//     and wakes the trigger on new mail (listen.go);
//   - Gmail watches and Microsoft Graph subscriptions: one replica at a time
//     (a lease) creates, renews and removes them at the provider
//     (reconcile.go), and the provider notifies public endpoints that any
//     replica serves (endpoints.go).
//
// A wake-up only asks for the trigger's normal check (trigger.Checker), which
// stays the single source of truth; periodic checks continue as a safety net.
package push

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/glue2flow/g2f/internal/netguard"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/secrets"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// Waker asks for an immediate check of a polling trigger (trigger.Checker).
type Waker interface {
	Wake(ctx context.Context, workflowID, nodeID string) error
}

// Contexts builds the context a check of a trigger gets (engine.Engine), so
// listeners resolve credentials exactly like the check.
type Contexts interface {
	TriggerContext(ctx context.Context, wf *workflow.Workflow, nodeID string) (*nodes.ExecContext, error)
}

// Tokens returns fresh access tokens of connected accounts (oauth.Manager).
type Tokens interface {
	Token(ctx context.Context, org, module, connectionID string) (*nodes.OAuthToken, error)
}

// Settings returns an organization's module settings (secrets.Service).
type Settings interface {
	ModuleSettings(ctx context.Context, org, module string) (config, vars map[string]any, err error)
}

type Options struct {
	Store    *store.Store
	Registry *nodes.Registry
	Box      *secrets.Box
	Waker    Waker
	Contexts Contexts
	Tokens   Tokens
	Settings Settings
	Log      *slog.Logger
	// Instance identifies this replica in leases (engine instance id).
	Instance string
	// PublicURL is where providers send notifications; it must be a public
	// https URL for Gmail and Graph.
	PublicURL string
	// Enabled is the master switch (G2F_MAIL_PUSH).
	Enabled bool
	// IdleMax caps the IMAP IDLE connections of this replica.
	IdleMax int

	// GoogleKeys verifies the OIDC tokens Pub/Sub push attaches; default:
	// Google's published keys.
	GoogleKeys oidc.KeySet
	// Timing, shortened by tests.
	ListenTick     time.Duration // lease renewal of listeners
	Resync         time.Duration // reload of active workflows
	ReconcileEvery time.Duration
	MinBackoff     time.Duration // listener reconnects
	MaxBackoff     time.Duration
	// UnsupportedRetry: how long a trigger whose server cannot push polls
	// before trying again.
	UnsupportedRetry time.Duration
}

// Service runs listeners and the reconciler and serves the endpoints.
type Service struct {
	opts Options
	st   *store.Store
	log  *slog.Logger

	mu     sync.Mutex
	active map[string]*workflow.Workflow // active workflows, by id
	loaded time.Time
	poke   chan struct{}
	pokeRe chan struct{}

	lis *listeners
}

const (
	googleIssuer = "https://accounts.google.com"
	googleCerts  = "https://www.googleapis.com/oauth2/v3/certs"
)

func New(o Options) *Service {
	o.IdleMax = max(o.IdleMax, 0) // 0: no IDLE connections
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.ListenTick, 10*time.Second)
	def(&o.Resync, 30*time.Second)
	def(&o.ReconcileEvery, 30*time.Second)
	def(&o.MinBackoff, time.Second)
	def(&o.MaxBackoff, 5*time.Minute)
	def(&o.UnsupportedRetry, time.Hour)
	if o.Log == nil {
		o.Log = slog.Default()
	}
	o.PublicURL = strings.TrimRight(o.PublicURL, "/")
	if o.GoogleKeys == nil { // fetched on first use, then cached
		o.GoogleKeys = oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(),
			&http.Client{Timeout: 10 * time.Second, Transport: netguard.Unguarded()}), googleCerts)
	}
	s := &Service{
		opts: o, st: o.Store, log: o.Log.With("component", "push"),
		active: map[string]*workflow.Workflow{}, poke: make(chan struct{}, 1), pokeRe: make(chan struct{}, 1),
	}
	s.lis = newListeners(s)
	return s
}

// Run keeps listeners and subscriptions converged until ctx ends; listeners
// are closed and their leases released before it returns.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.lis.run(ctx) }()
	go func() { defer wg.Done(); s.reconcileLoop(ctx) }()
	wg.Wait()
}

// WorkflowChanged implements api.ChangeListener.
func (s *Service) WorkflowChanged(w *workflow.Workflow) {
	s.mu.Lock()
	if w.Active {
		s.active[w.ID] = w
	} else {
		delete(s.active, w.ID)
	}
	s.mu.Unlock()
	s.pokeAll()
}

// WorkflowDeleted implements api.ChangeListener.
func (s *Service) WorkflowDeleted(id string) {
	s.mu.Lock()
	delete(s.active, id)
	s.mu.Unlock()
	s.pokeAll()
}

func (s *Service) pokeAll() {
	for _, ch := range []chan struct{}{s.poke, s.pokeRe} {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// activeWorkflows returns the active workflows, reloading them from the
// database when the copy is older than Resync (changes on other replicas).
func (s *Service) activeWorkflows(ctx context.Context) ([]*workflow.Workflow, error) {
	s.mu.Lock()
	stale := time.Since(s.loaded) >= s.opts.Resync
	s.mu.Unlock()
	if stale {
		wfs, err := s.st.ListActiveWorkflows(ctx)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.active = map[string]*workflow.Workflow{}
		for _, w := range wfs {
			s.active[w.ID] = w
		}
		s.loaded = time.Now()
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*workflow.Workflow, 0, len(s.active))
	for _, w := range s.active {
		out = append(out, w)
	}
	return out, nil
}

// trigger is an active, enabled polling trigger that may use push.
type trigger struct {
	wf   *workflow.Workflow
	node *workflow.Node
	def  nodes.Definition
	impl nodes.Node
}

func (t trigger) ref() store.TriggerRef {
	return store.TriggerRef{WorkflowID: t.wf.ID, NodeID: t.node.ID}
}

// pushTriggers lists the triggers of active workflows that support push
// through endpoint and have it turned on.
func (s *Service) pushTriggers(wfs []*workflow.Workflow, endpoints ...string) []trigger {
	var out []trigger
	for _, w := range wfs {
		for i := range w.Graph.Nodes {
			n := &w.Graph.Nodes[i]
			if n.Disabled || !nodes.PushAllowed(n.Params) {
				continue
			}
			impl, ok := s.opts.Registry.Get(n.Type)
			if !ok {
				continue
			}
			def := impl.Definition()
			if def.Trigger != nodes.TriggerPoll || def.Push == "" {
				continue
			}
			for _, e := range endpoints {
				if def.Push == e {
					out = append(out, trigger{wf: w, node: n, def: def, impl: impl})
				}
			}
		}
	}
	return out
}

// publicHTTPS reports whether providers can reach the server.
func (s *Service) publicHTTPS() bool { return nodes.HTTPSURL(s.opts.PublicURL) }

// --- endpoint tokens --------------------------------------------------------

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func tokenAAD(org, endpoint string) string { return "push-token/" + org + "/" + endpoint }

// endpointToken returns (creating on first use) the organization's secret
// path token of a push endpoint.
func (s *Service) endpointToken(ctx context.Context, org, endpoint string) (string, error) {
	sealed, err := s.st.PushToken(ctx, org, endpoint)
	if errors.Is(err, store.ErrNotFound) {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		token := base64.RawURLEncoding.EncodeToString(b)
		if sealed, err = s.opts.Box.Seal(token, tokenAAD(org, endpoint)); err != nil {
			return "", err
		}
		sealed, err = s.st.CreatePushToken(ctx, org, endpoint, tokenHash(token), sealed)
	}
	if err != nil {
		return "", err
	}
	return s.opts.Box.Open(sealed, tokenAAD(org, endpoint))
}

// GmailEndpoint is the URL the organization's Pub/Sub push subscription must call.
func (s *Service) GmailEndpoint(ctx context.Context, org string) (string, error) {
	token, err := s.endpointToken(ctx, org, nodes.PushGmail)
	if err != nil {
		return "", err
	}
	return s.opts.PublicURL + "/push/gmail/" + token, nil
}

// GraphEndpoint is the notification URL of Microsoft Graph subscriptions.
func (s *Service) GraphEndpoint() string { return s.opts.PublicURL + "/push/graph" }

// --- delivery state, for the editor -----------------------------------------

// Delivery describes how a polling trigger learns about new mail.
type Delivery struct {
	// Push is true while notifications arrive (the interval is then only a
	// safety net).
	Push bool `json:"push"`
	// Via names the push source: IMAP IDLE, Gmail or Microsoft Graph.
	Via    string     `json:"via"`
	Since  *time.Time `json:"since,omitempty"`
	Renews *time.Time `json:"renews,omitempty"`
	// Reason explains why the trigger polls.
	Reason string `json:"reason,omitempty"`
	// Endpoint is the Pub/Sub push endpoint to configure (admins, Gmail).
	Endpoint string `json:"endpoint,omitempty"`
}

var viaNames = map[string]string{nodes.PushListen: "IMAP IDLE", nodes.PushGmail: "Gmail", nodes.PushGraph: "Microsoft Graph"}

// Delivery reports the push state of a polling trigger; nil if its node
// type cannot push. admin adds the Gmail endpoint URL.
func (s *Service) Delivery(ctx context.Context, wf *workflow.Workflow, n *workflow.Node, admin bool) *Delivery {
	impl, ok := s.opts.Registry.Get(n.Type)
	if !ok {
		return nil
	}
	def := impl.Definition()
	if def.Trigger != nodes.TriggerPoll || def.Push == "" {
		return nil
	}
	d := &Delivery{Via: viaNames[def.Push]}
	if admin && def.Push == nodes.PushGmail {
		if u, err := s.GmailEndpoint(ctx, wf.OrgID); err == nil {
			d.Endpoint = u
		}
	}
	switch {
	case !s.opts.Enabled:
		d.Reason = "push is turned off on this server (G2F_MAIL_PUSH=false)"
		return d
	case !nodes.PushAllowed(n.Params):
		d.Reason = "push is turned off for this trigger (Delivery: polling only)"
		return d
	case !wf.Active || n.Disabled:
		d.Reason = "the workflow is not active"
		return d
	}
	if def.Push == nodes.PushListen {
		l, err := s.st.GetLease(ctx, listenLease(store.TriggerRef{WorkflowID: wf.ID, NodeID: n.ID}))
		switch {
		case errors.Is(err, store.ErrNotFound):
			d.Reason = fmt.Sprintf("not connected yet: connecting, or every replica already keeps G2F_IMAP_IDLE_MAX (%d here) IDLE connections", s.opts.IdleMax)
		case err != nil:
			d.Reason = "push state unavailable: " + err.Error()
		case l.State == stateConnected:
			d.Push, d.Since = true, l.StateSince
		case l.State == stateUnsupported:
			d.Reason = "the IMAP server does not support IDLE; trying again later"
		case l.State == stateError:
			d.Reason = "IDLE connection failed (" + l.Detail + "); reconnecting"
		default:
			d.Reason = "connecting to the IMAP server"
		}
		return d
	}
	spec := impl.(nodes.Pusher).PushSpec()
	if !s.publicHTTPS() {
		d.Reason = fmt.Sprintf("G2F_PUBLIC_URL (%s) is not a public https URL, so %s cannot notify this server", s.opts.PublicURL, d.Via)
		return d
	}
	if missing := s.missingConfig(ctx, wf.OrgID, def.Module, spec); missing != "" {
		d.Reason = "push is not set up: an admin must set " + missing
		return d
	}
	conn, _ := n.Params[nodes.ConnectionParam].(string)
	if conn == "" {
		d.Reason = "no account selected"
		return d
	}
	p, err := s.st.GetPushSubscription(ctx, subKey(def.Module, spec, wf.ID, n.ID, conn))
	switch {
	case errors.Is(err, store.ErrNotFound):
		d.Reason = "subscribing at " + d.Via
	case err != nil:
		d.Reason = "push state unavailable: " + err.Error()
	case p.LastError != "":
		d.Reason = "subscription failed: " + p.LastError
		if p.RetryAt != nil {
			d.Reason += " (retrying " + p.RetryAt.UTC().Format(time.RFC3339) + ")"
		}
	case p.ExternalID == "" || p.ExpiresAt == nil || !p.ExpiresAt.After(time.Now()):
		d.Reason = "subscribing at " + d.Via
	default:
		d.Push = true
		d.Since = &p.CreatedAt
		if r := renewAt(p, spec); !r.IsZero() {
			d.Renews = &r
		}
	}
	return d
}

// missingConfig names the first config entry push needs that is not set.
func (s *Service) missingConfig(ctx context.Context, org, module string, spec *nodes.PushSpec) string {
	if len(spec.Requires) == 0 {
		return ""
	}
	cfg, _, err := s.opts.Settings.ModuleSettings(ctx, org, module)
	if err != nil {
		return "the plugin settings (unreadable: " + err.Error() + ")"
	}
	info, _ := s.opts.Registry.Module(module)
	for _, r := range spec.Requires {
		if v, _ := cfg[r].(string); strings.TrimSpace(v) == "" {
			label, name := r, module
			if info != nil {
				name = info.Name
				for _, p := range info.Config {
					if p.Name == r && p.Label != "" {
						label = p.Label
					}
				}
			}
			return fmt.Sprintf("%q in Plugins → %s → Settings", label, name)
		}
	}
	return ""
}
