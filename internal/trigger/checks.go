package trigger

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// CheckStore serializes the checks of polling triggers across replicas (see
// store.RequestCheck). *store.Store implements it.
type CheckStore interface {
	RequestCheck(ctx context.Context, workflowID, nodeID string) error
	RequestSlot(ctx context.Context, workflowID, nodeID string, slot time.Time) (bool, error)
	AcquireCheck(ctx context.Context, workflowID, nodeID, owner string, ttl time.Duration) (bool, error)
	ReleaseCheck(ctx context.Context, workflowID, nodeID, owner string, done bool) (bool, error)
	RenewChecks(ctx context.Context, owner string, ttl time.Duration) error
	PendingChecks(ctx context.Context, limit int) ([]store.TriggerRef, error)
}

const (
	// checkLease is how long a check's claim lasts without renewal. Running
	// checks are renewed every sweep, so the claims of a stopped replica
	// expire soon and its requests are picked up by another one.
	checkLease = time.Minute
	// checkSweep: how often requests that nobody runs are picked up.
	checkSweep = 5 * time.Second
	// startAttempts: a run that cannot be created (database hiccup) is
	// retried, because the check has already remembered its items.
	startAttempts = 3
)

// Checker runs the checks of polling triggers, whoever asks for them (the
// scheduler, push notifications, IMAP IDLE wake-ups):
//
//   - one check of a trigger at a time across all replicas (a claim per
//     trigger in PostgreSQL), so two checks never read the same cursor;
//   - requests made while a check runs cause exactly one more check
//     afterwards, and bursts of requests coalesce into one or two checks;
//   - a request is cleared only by a check that started after it, so when a
//     replica fails or stops before the check ran, another one runs it;
//   - checks run on a fixed number of workers.
type Checker struct {
	store   CheckStore
	load    func(ctx context.Context, id string) (*workflow.Workflow, error)
	eng     Engine
	reg     *nodes.Registry
	log     *slog.Logger
	owner   string
	workers int
	sweep   time.Duration

	mu      sync.Mutex
	queue   []store.TriggerRef
	queued  map[store.TriggerRef]bool
	running map[store.TriggerRef]bool
	dirty   map[store.TriggerRef]bool // requested again while running here
	signal  chan struct{}
}

func newChecker(st CheckStore, load func(context.Context, string) (*workflow.Workflow, error), eng Engine,
	reg *nodes.Registry, log *slog.Logger, owner string, workers int) *Checker {
	if workers <= 0 {
		workers = 8
	}
	return &Checker{
		store: st, load: load, eng: eng, reg: reg, log: log, owner: owner, workers: workers, sweep: checkSweep,
		queued: map[store.TriggerRef]bool{}, running: map[store.TriggerRef]bool{}, dirty: map[store.TriggerRef]bool{},
		signal: make(chan struct{}, 1),
	}
}

// Wake asks for a check of a polling trigger now. It returns once the
// request is stored; the check follows on some replica.
func (c *Checker) Wake(ctx context.Context, workflowID, nodeID string) error {
	if err := c.store.RequestCheck(ctx, workflowID, nodeID); err != nil {
		return err
	}
	c.enqueue(store.TriggerRef{WorkflowID: workflowID, NodeID: nodeID})
	return nil
}

// enqueue schedules a local attempt to run a requested check.
func (c *Checker) enqueue(ref store.TriggerRef) {
	c.mu.Lock()
	switch {
	case c.running[ref]:
		c.dirty[ref] = true
	case !c.queued[ref]:
		c.queued[ref] = true
		c.queue = append(c.queue, ref)
	}
	c.mu.Unlock()
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

// next takes the next queued trigger, or returns false when ctx ends.
func (c *Checker) next(ctx context.Context) (store.TriggerRef, bool) {
	for {
		c.mu.Lock()
		if len(c.queue) > 0 {
			ref := c.queue[0]
			c.queue = c.queue[1:]
			delete(c.queued, ref)
			c.running[ref] = true
			more := len(c.queue) > 0
			c.mu.Unlock()
			if more { // let the next idle worker continue
				select {
				case c.signal <- struct{}{}:
				default:
				}
			}
			return ref, true
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return store.TriggerRef{}, false
		case <-c.signal:
		}
	}
}

// Run starts the workers and picks up open requests until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range c.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ref, ok := c.next(ctx)
				if !ok {
					return
				}
				c.runOne(ctx, ref)
			}
		}()
	}
	t := time.NewTicker(c.sweep)
	defer t.Stop()
	for {
		c.sweepOnce(ctx)
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
		}
	}
}

// sweepOnce renews this replica's claims and queues requests nobody runs.
func (c *Checker) sweepOnce(ctx context.Context) {
	if err := c.store.RenewChecks(ctx, c.owner, checkLease); err != nil && ctx.Err() == nil {
		c.log.Error("renew check claims", "err", err)
	}
	c.mu.Lock()
	free := c.workers - len(c.running) - len(c.queue)
	c.mu.Unlock()
	if free <= 0 {
		return
	}
	refs, err := c.store.PendingChecks(ctx, free)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Error("load requested checks", "err", err)
		}
		return
	}
	for _, r := range refs {
		c.enqueue(r)
	}
}

// runOne claims and runs a requested check, and queues the follow-up if
// requests arrived meanwhile.
func (c *Checker) runOne(ctx context.Context, ref store.TriggerRef) {
	log := c.log.With("workflow", ref.WorkflowID, "node", ref.NodeID)
	ok, err := c.store.AcquireCheck(ctx, ref.WorkflowID, ref.NodeID, c.owner, checkLease)
	if err != nil && ctx.Err() == nil {
		log.Error("claim check", "err", err) // the request stays; a later sweep retries
	}
	done, again := false, false
	if ok {
		done = c.check(ctx, ref, log)
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		again, err = c.store.ReleaseCheck(rctx, ref.WorkflowID, ref.NodeID, c.owner, done)
		cancel()
		if err != nil {
			log.Error("release check", "err", err) // the claim expires; the request stays
		}
	}
	c.mu.Lock()
	delete(c.running, ref)
	dirty := c.dirty[ref]
	delete(c.dirty, ref)
	c.mu.Unlock()
	// Without done the request stays open, and a sweep retries it after a pause.
	if ctx.Err() == nil && ((done && again) || dirty) {
		c.enqueue(ref)
	}
}

// check runs one check and starts a run with its new items. It reports
// whether the request was answered: false only if the check could not be
// started (it is retried), true also when the trigger no longer listens.
func (c *Checker) check(ctx context.Context, ref store.TriggerRef, log *slog.Logger) bool {
	w, err := c.load(ctx, ref.WorkflowID)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Error("load workflow", "err", err)
		}
		return false
	}
	if n := w.Graph.Node(ref.NodeID); !w.Active || n == nil || n.Disabled || c.reg.TriggerKind(n.Type) != nodes.TriggerPoll {
		return true // changed since the check was requested
	}
	items, err := c.eng.Poll(ctx, w, ref.NodeID)
	if err != nil {
		if ctx.Err() != nil {
			return false // shutting down: another replica checks
		}
		log.Warn("polling check failed", "err", err) // recorded by the engine
		return true
	}
	if len(items) == 0 {
		return true
	}
	for attempt := 1; ; attempt++ {
		exec, err := c.eng.Start(context.WithoutCancel(ctx), engine.Request{
			Workflow: w, TriggerType: nodes.TriggerPoll, TriggerNode: ref.NodeID, TriggeredBy: "poll", Items: items,
		})
		if err == nil {
			log.Info("polling trigger found new items", "items", len(items), "execution", exec.ID)
			return true
		}
		if attempt == startAttempts {
			log.Error("start polled execution; the new items are lost", "items", len(items), "err", err)
			return true
		}
		log.Warn("start polled execution, retrying", "err", err)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
}
