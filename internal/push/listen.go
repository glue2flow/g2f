package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// Listener states (lease state, shown in the editor).
const (
	stateConnecting  = "connecting"
	stateConnected   = "connected"
	stateError       = "error"
	stateUnsupported = "unsupported"
)

// listenLease is the lease of the replica listening for a trigger.
func listenLease(r store.TriggerRef) string { return "listen:" + r.WorkflowID + "/" + r.NodeID }

// leaseTTL: a listener lease not renewed for this long is taken over.
func (l *listeners) leaseTTL() time.Duration { return 3 * l.s.opts.ListenTick }

// stableAfter: a connection that lasted this long resets the backoff.
const stableAfter = time.Minute

// listeners keeps one connection per trigger whose node type has a
// listener (IMAP IDLE) across all replicas: whoever holds the trigger's
// lease listens, renews the lease every tick and reconnects with backoff.
// A replica holds at most IdleMax open listeners; triggers beyond that (on
// every replica) keep polling at their interval.
type listeners struct {
	s *Service

	// slots holds one token per open (or opening) connection: at most IdleMax.
	slots chan struct{}

	mu      sync.Mutex
	running map[store.TriggerRef]*listener
}

type listener struct {
	ref         store.TriggerRef
	fingerprint string
	cancel      context.CancelFunc
	done        chan struct{}
}

func newListeners(s *Service) *listeners {
	return &listeners{s: s, running: map[store.TriggerRef]*listener{}, slots: make(chan struct{}, s.opts.IdleMax)}
}

func (l *listeners) run(ctx context.Context) {
	t := time.NewTicker(l.s.opts.ListenTick)
	defer t.Stop()
	for {
		l.step(ctx)
		select {
		case <-ctx.Done():
			l.stopAll()
			return
		case <-t.C:
		case <-l.s.poke:
		}
	}
}

type wanted struct {
	t           trigger
	fingerprint string
}

// desired lists the triggers that should have a listener.
func (l *listeners) desired(ctx context.Context) (map[store.TriggerRef]wanted, error) {
	out := map[store.TriggerRef]wanted{}
	if !l.s.opts.Enabled {
		return out, nil
	}
	wfs, err := l.s.activeWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	settings := map[string]map[string]any{}
	for _, t := range l.s.pushTriggers(wfs, nodes.PushListen) {
		if _, ok := nodes.Listener(t.node.Type); !ok {
			continue
		}
		key := t.wf.OrgID + "\x00" + t.def.Module
		cfg, ok := settings[key]
		if !ok {
			if cfg, _, err = l.s.opts.Settings.ModuleSettings(ctx, t.wf.OrgID, t.def.Module); err != nil {
				return nil, err
			}
			settings[key] = cfg
		}
		out[t.ref()] = wanted{t: t, fingerprint: listenFingerprint(t.node, cfg)}
	}
	return out, nil
}

// listenFingerprint changes when the node's settings or the module settings
// change, so the listener reconnects with them.
func listenFingerprint(n *workflow.Node, cfg map[string]any) string {
	params := map[string]any{}
	for k, v := range n.Params {
		if k != nodes.PollParam && k != nodes.DeliveryParam {
			params[k] = v
		}
	}
	b, _ := json.Marshal([]any{n.Type, params, cfg})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// step converges the running listeners with the desired ones.
func (l *listeners) step(ctx context.Context) {
	want, err := l.desired(ctx)
	if err != nil {
		if ctx.Err() == nil {
			l.s.log.Error("listeners: load triggers", "err", err)
		}
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for ref, x := range l.running {
		if w, ok := want[ref]; !ok || w.fingerprint != x.fingerprint {
			l.stopLocked(x)
		}
	}
	// Renew the leases held; a listener that lost its lease stops.
	if len(l.running) > 0 {
		names := make([]string, 0, len(l.running))
		for ref := range l.running {
			names = append(names, listenLease(ref))
		}
		held, err := l.s.st.RenewLeases(ctx, l.s.opts.Instance, names, l.leaseTTL())
		if err != nil {
			if ctx.Err() == nil {
				l.s.log.Error("listeners: renew leases", "err", err)
			}
			return
		}
		ok := map[string]bool{}
		for _, n := range held {
			ok[n] = true
		}
		for ref, x := range l.running {
			if !ok[listenLease(ref)] {
				l.s.log.Warn("listener lease lost", "workflow", ref.WorkflowID, "node", ref.NodeID)
				l.stopLocked(x)
			}
		}
	}
	// Start missing listeners, in a stable order, up to the cap.
	refs := make([]store.TriggerRef, 0, len(want))
	for ref := range want {
		if l.running[ref] == nil {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(a, b int) bool { return listenLease(refs[a]) < listenLease(refs[b]) })
	for _, ref := range refs {
		select {
		case l.slots <- struct{}{}: // handed to the listener
		default:
			return // at the cap: other replicas or polling take the rest
		}
		got, err := l.s.st.AcquireLease(ctx, listenLease(ref), l.s.opts.Instance, l.leaseTTL())
		if err != nil || !got {
			<-l.slots
			if err != nil && ctx.Err() == nil {
				l.s.log.Error("listeners: acquire lease", "err", err)
			}
			if err != nil {
				return
			}
			continue
		}
		l.startLocked(ctx, want[ref])
	}
}

func (l *listeners) startLocked(parent context.Context, w wanted) {
	ctx, cancel := context.WithCancel(parent)
	x := &listener{ref: w.t.ref(), fingerprint: w.fingerprint, cancel: cancel, done: make(chan struct{})}
	l.running[x.ref] = x
	go func() {
		defer close(x.done)
		l.listen(ctx, x, w.t.wf)
	}()
}

// stopLocked closes a listener and gives its lease up.
func (l *listeners) stopLocked(x *listener) {
	x.cancel()
	<-x.done
	delete(l.running, x.ref)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.s.st.ReleaseLease(ctx, listenLease(x.ref), l.s.opts.Instance); err != nil {
		l.s.log.Error("listeners: release lease", "err", err)
	}
}

func (l *listeners) stopAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.running {
		l.stopLocked(x)
	}
}

// listen runs the trigger's listener until ctx ends, reconnecting with
// exponential backoff (with jitter) and retrying servers that cannot push
// after UnsupportedRetry. After every (re)connect it wakes the trigger once,
// because mail may have arrived while it was not listening. It starts with
// a connection slot and gives it back while it waits.
func (l *listeners) listen(ctx context.Context, x *listener, wf *workflow.Workflow) {
	hasSlot := true
	defer func() {
		if hasSlot {
			<-l.slots
		}
	}()
	fn, _ := nodes.Listener(wf.Graph.Node(x.ref.NodeID).Type)
	name := listenLease(x.ref)
	log := l.s.log.With("workflow", x.ref.WorkflowID, "node", x.ref.NodeID)
	setState := func(state, detail string) {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := l.s.st.SetLeaseState(sctx, name, l.s.opts.Instance, state, detail); err != nil {
			log.Error("listener: record state", "err", err)
		}
	}
	// Wake-ups are handed to a goroutine and coalesce while it is busy, so a
	// burst of notifications neither blocks the connection nor floods the
	// database.
	pending := make(chan struct{}, 1)
	wake := func() {
		select {
		case pending <- struct{}{}:
		default:
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pending:
				if err := l.s.opts.Waker.Wake(ctx, x.ref.WorkflowID, x.ref.NodeID); err != nil && ctx.Err() == nil {
					log.Error("listener: wake trigger", "err", err)
				}
			}
		}
	}()
	backoff := l.s.opts.MinBackoff
	for ctx.Err() == nil {
		setState(stateConnecting, "")
		var connectedAt time.Time
		ec, err := l.s.opts.Contexts.TriggerContext(ctx, wf, x.ref.NodeID)
		if err == nil {
			err = fn(ctx, ec, wake, func() {
				connectedAt = time.Now()
				setState(stateConnected, "")
				log.Info("listening for new mail")
				wake()
			})
		}
		if ctx.Err() != nil {
			return
		}
		<-l.slots
		hasSlot = false
		wait := backoff
		if errors.Is(err, nodes.ErrPushUnsupported) {
			log.Info("server cannot push; polling", "err", err)
			setState(stateUnsupported, err.Error())
			wait = l.s.opts.UnsupportedRetry
		} else {
			if err == nil {
				err = errors.New("connection closed")
			}
			log.Warn("listener disconnected", "err", err, "retryIn", backoff)
			setState(stateError, err.Error())
			if !connectedAt.IsZero() && time.Since(connectedAt) >= stableAfter {
				wait = l.s.opts.MinBackoff
			}
			backoff = min(wait*2, l.s.opts.MaxBackoff)
			wait += time.Duration(rand.Int64N(int64(wait)/4 + 1))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		select {
		case <-ctx.Done():
			return
		case l.slots <- struct{}{}:
			hasSlot = true
		}
	}
}
