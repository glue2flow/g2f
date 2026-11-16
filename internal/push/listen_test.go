package push

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

func (f *fixture) idleTrigger(t *testing.T, p *imapProxy) *workflow.Workflow {
	t.Helper()
	f.loadModuleOnce(t)
	f.settings.set("acme", "email", p.config())
	return f.workflow(t, "email.received", map[string]any{})
}

func (f *fixture) loadModuleOnce(t *testing.T) {
	if _, ok := f.reg.Module("email"); !ok {
		f.loadModule(t, "email/plugin.yaml")
	}
}

func (f *fixture) leaseState(wf *workflow.Workflow) string {
	l, err := f.st.GetLease(context.Background(), listenLease(store.TriggerRef{WorkflowID: wf.ID, NodeID: "t"}))
	if err != nil {
		return ""
	}
	return l.State
}

func TestIdleWakesTriggerOnNewMail(t *testing.T) {
	f := newFixture(t)
	p, deliver := startIMAP(t)
	wf := f.idleTrigger(t, p)
	s, _ := f.service(t, nil)

	eventually(t, "IDLE connection", func() bool { return f.leaseState(wf) == stateConnected })
	// Connecting wakes the trigger once: mail may have arrived meanwhile.
	eventually(t, "wake after connecting", func() bool { return f.waker.count(wf.ID, "t") == 1 })
	d := s.Delivery(context.Background(), wf, &wf.Graph.Nodes[0], false)
	if !d.Push || d.Via != "IMAP IDLE" || d.Since == nil {
		t.Fatalf("delivery: %+v", d)
	}
	healthy, err := f.st.HealthyPushTriggers(context.Background())
	if err != nil || len(healthy) != 1 || healthy[0].WorkflowID != wf.ID {
		t.Fatalf("healthy: %v %v", healthy, err)
	}

	start := time.Now()
	deliver("Hello")
	eventually(t, "wake on new mail", func() bool { return f.waker.count(wf.ID, "t") == 2 })
	if time.Since(start) > 2*time.Second {
		t.Fatalf("wake-up took %v", time.Since(start))
	}

	// Deactivation closes the connection and frees the lease.
	wf.Active = false
	if err := f.st.UpdateWorkflow(context.Background(), wf, nil); err != nil {
		t.Fatal(err)
	}
	s.WorkflowChanged(wf)
	eventually(t, "lease released", func() bool { return f.leaseState(wf) == "" })
	if d := s.Delivery(context.Background(), wf, &wf.Graph.Nodes[0], false); d.Push {
		t.Fatalf("still push after deactivation: %+v", d)
	}
}

// Only one replica listens; when it stops, another takes over. A lease left
// by a crashed replica is taken over once it expires.
func TestIdleLeaseTakeover(t *testing.T) {
	f := newFixture(t)
	p, _ := startIMAP(t)
	wf := f.idleTrigger(t, p)
	ref := store.TriggerRef{WorkflowID: wf.ID, NodeID: "t"}

	// A crashed replica's lease: never renewed.
	ghost := uuid.NewString()
	if ok, err := f.st.AcquireLease(context.Background(), listenLease(ref), ghost, 300*time.Millisecond); err != nil || !ok {
		t.Fatal(ok, err)
	}
	a, stopA := f.service(t, nil)
	b, stopB := f.service(t, nil)
	holder := func() string {
		l, err := f.st.GetLease(context.Background(), listenLease(ref))
		if err != nil || l.State != stateConnected {
			return ""
		}
		return l.Holder
	}
	eventually(t, "takeover of the expired lease", func() bool { h := holder(); return h != "" && h != ghost })
	time.Sleep(200 * time.Millisecond)
	a.lis.mu.Lock()
	na := len(a.lis.running)
	a.lis.mu.Unlock()
	b.lis.mu.Lock()
	nb := len(b.lis.running)
	b.lis.mu.Unlock()
	if na+nb != 1 {
		t.Fatalf("listeners on replicas: %d + %d, want exactly one", na, nb)
	}
	first := holder()
	if first == a.opts.Instance {
		stopA()
	} else {
		stopB()
	}
	eventually(t, "the other replica takes over", func() bool { h := holder(); return h != "" && h != first })
}

// A dropped connection is re-established; while the server is unreachable
// the attempts back off exponentially (capped).
func TestIdleReconnectsWithBackoff(t *testing.T) {
	f := newFixture(t)
	p, deliver := startIMAP(t)
	wf := f.idleTrigger(t, p)
	f.service(t, nil)
	eventually(t, "IDLE connection", func() bool { return f.leaseState(wf) == stateConnected })

	p.refuse.Store(true)
	p.drop()
	eventually(t, "error state", func() bool { return f.leaseState(wf) == stateError })
	before := len(p.acceptTimes())
	time.Sleep(900 * time.Millisecond)
	times := p.acceptTimes()[before:]
	if len(times) < 4 {
		t.Fatalf("only %d reconnect attempts", len(times))
	}
	var gaps []time.Duration
	for i := 1; i < len(times); i++ {
		gaps = append(gaps, times[i].Sub(times[i-1]))
	}
	for _, g := range gaps {
		if g < 15*time.Millisecond || g > 400*time.Millisecond {
			t.Fatalf("reconnect gaps %v outside [min backoff, max backoff + jitter]", gaps)
		}
	}
	if gaps[len(gaps)-1] < 2*gaps[0] {
		t.Fatalf("no exponential backoff: %v", gaps)
	}

	p.refuse.Store(false)
	eventually(t, "reconnected", func() bool { return f.leaseState(wf) == stateConnected })
	n := f.waker.count(wf.ID, "t")
	deliver("after reconnect")
	eventually(t, "wake after reconnect", func() bool { return f.waker.count(wf.ID, "t") > n })
}

// Servers without IDLE: the trigger keeps polling, says why, and holds no
// connection slot.
func TestIdleUnsupportedFallsBackToPolling(t *testing.T) {
	f := newFixture(t)
	p, _ := startIMAP(t)
	p.stripIdle.Store(true)
	wf := f.idleTrigger(t, p)
	s, _ := f.service(t, nil)
	eventually(t, "unsupported state", func() bool { return f.leaseState(wf) == stateUnsupported })
	d := s.Delivery(context.Background(), wf, &wf.Graph.Nodes[0], false)
	if d.Push || !strings.Contains(d.Reason, "does not support IDLE") {
		t.Fatalf("delivery: %+v", d)
	}
	if h, _ := f.st.HealthyPushTriggers(context.Background()); len(h) != 0 {
		t.Fatalf("healthy: %v", h)
	}
	eventually(t, "slot freed", func() bool { return len(s.lis.slots) == 0 })
}

// A replica keeps at most IdleMax connections; the rest is left to other
// replicas, or polls.
func TestIdleConnectionCap(t *testing.T) {
	f := newFixture(t)
	p, _ := startIMAP(t)
	wf1 := f.idleTrigger(t, p)
	wf2 := f.workflow(t, "email.received", map[string]any{})
	a, _ := f.service(t, func(o *Options) { o.IdleMax = 1 })
	eventually(t, "one connection", func() bool {
		return (f.leaseState(wf1) == stateConnected) != (f.leaseState(wf2) == stateConnected)
	})
	time.Sleep(200 * time.Millisecond)
	if f.leaseState(wf1) == stateConnected && f.leaseState(wf2) == stateConnected {
		t.Fatal("cap exceeded")
	}
	waiting := wf2
	if f.leaseState(wf2) == stateConnected {
		waiting = wf1
	}
	if d := a.Delivery(context.Background(), waiting, &waiting.Graph.Nodes[0], false); d.Push || !strings.Contains(d.Reason, "G2F_IMAP_IDLE_MAX") {
		t.Fatalf("delivery of the trigger beyond the cap: %+v", d)
	}
	f.service(t, func(o *Options) { o.IdleMax = 1 })
	eventually(t, "second replica takes the rest", func() bool {
		return f.leaseState(wf1) == stateConnected && f.leaseState(wf2) == stateConnected
	})
}

// A server that accepts but never answers does not block shutdown.
func TestIdleStalledServerShutdown(t *testing.T) {
	f := newFixture(t)
	p, _ := startIMAP(t)
	p.stall.Store(true)
	wf := f.idleTrigger(t, p)
	_, stop := f.service(t, nil)
	eventually(t, "connecting", func() bool { return f.leaseState(wf) == stateConnecting && p.accepted.Load() > 0 })
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked by a stalled server")
	}
	if _, err := f.st.GetLease(context.Background(), listenLease(store.TriggerRef{WorkflowID: wf.ID, NodeID: "t"})); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease not released on shutdown: %v", err)
	}
}

// Push turned off per trigger or on the server: no listener.
func TestIdleOff(t *testing.T) {
	f := newFixture(t)
	p, _ := startIMAP(t)
	f.loadModuleOnce(t)
	f.settings.set("acme", "email", p.config())
	wf := f.workflow(t, "email.received", map[string]any{"delivery": "poll"})
	s, _ := f.service(t, nil)
	time.Sleep(300 * time.Millisecond)
	if p.accepted.Load() != 0 {
		t.Fatal("connected despite Delivery: polling only")
	}
	if d := s.Delivery(context.Background(), wf, &wf.Graph.Nodes[0], false); d.Push || !strings.Contains(d.Reason, "polling only") {
		t.Fatalf("delivery: %+v", d)
	}
	wf2 := f.workflow(t, "email.received", map[string]any{})
	s2, _ := f.service(t, func(o *Options) { o.Enabled = false })
	if d := s2.Delivery(context.Background(), wf2, &wf2.Graph.Nodes[0], false); d.Push || !strings.Contains(d.Reason, "G2F_MAIL_PUSH") {
		t.Fatalf("delivery: %+v", d)
	}
}
