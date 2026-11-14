// Package trigger starts workflows from schedules, polling triggers and webhooks.
package trigger

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// Store is the persistence the trigger package needs.
type Store interface {
	ListActiveWorkflows(ctx context.Context) ([]*workflow.Workflow, error)
	GetWorkflow(ctx context.Context, id string) (*workflow.Workflow, error)
	ClaimFiring(ctx context.Context, workflowID, nodeID, key string) (bool, error)
	HasFired(ctx context.Context, workflowID, nodeID, key string) (bool, error)
	PruneFirings(ctx context.Context, age time.Duration) (int64, error)
	CheckStore
}

// PushHealth reports which polling triggers currently receive push
// notifications; they are polled less often. *store.Store implements it.
type PushHealth interface {
	HealthyPushTriggers(ctx context.Context) ([]store.TriggerRef, error)
}

// Engine starts executions.
type Engine interface {
	Start(ctx context.Context, req engine.Request) (*store.Execution, error)
	Run(ctx context.Context, req engine.Request) (*engine.Result, error)
	// Poll runs one check of a polling trigger and returns the new items.
	// It records failed checks as failed executions itself.
	Poll(ctx context.Context, wf *workflow.Workflow, nodeID string) ([]nodes.Item, error)
}

// statePruner is implemented by stores that keep workflow state.
type statePruner interface {
	PruneState(ctx context.Context) (int64, error)
}

// executionPruner is implemented by stores that can delete old executions.
type executionPruner interface {
	PruneExecutions(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
}

const (
	// pruneBatch is how many executions one delete statement removes.
	pruneBatch = 1000
	// pruneMaxBatches bounds the work of one prune tick.
	pruneMaxBatches = 50
)

type SchedulerOptions struct {
	// ExecutionRetention: finished executions older than this are deleted.
	// 0 keeps them forever.
	ExecutionRetention time.Duration

	// CatchUp: a one-time schedule missed (e.g. during downtime) by at most this
	// long still fires once the server is back. Recurring schedules never catch up.
	CatchUp time.Duration
	// Resync: how often the active workflow set is reloaded from the database,
	// which picks up changes made through other replicas.
	Resync time.Duration

	// Instance identifies this replica in check claims (engine instance id).
	Instance string
	// CheckWorkers bounds how many polling checks run at once on this replica.
	CheckWorkers int
	// Health is optional; without it every trigger polls at its interval.
	Health PushHealth
}

// PushSafetyNet is the longest interval between checks of a trigger whose
// push delivery is healthy: notifications can be lost, the check catches up.
const PushSafetyNet = 15 * time.Minute

// EffectiveInterval is the interval between checks of a polling trigger
// configured to d: while push works, at least PushSafetyNet.
func EffectiveInterval(d time.Duration, pushHealthy bool) time.Duration {
	if pushHealthy {
		return max(d, PushSafetyNet)
	}
	return d
}

// pollOffset spreads polling triggers over their interval: a stable offset
// per trigger, the same on every replica, so checks do not all start at the
// same second.
func pollOffset(workflowID, nodeID string, d time.Duration) time.Duration {
	secs := uint64(d / time.Second)
	if secs == 0 {
		return 0
	}
	h := fnv.New64a()
	h.Write([]byte(workflowID + "/" + nodeID))
	return time.Duration(h.Sum64()%secs) * time.Second
}

// NextPoll returns the next scheduled check of a trigger after t.
func NextPoll(workflowID, nodeID string, d time.Duration, t time.Time) time.Time {
	off := pollOffset(workflowID, nodeID, d)
	return nextSlot(t.Add(-off), d).Add(off)
}

// Scheduler fires schedule triggers and requests the checks of polling
// triggers of active workflows.
//
// Every replica may run a Scheduler: each firing is claimed in the database
// with a key derived from the scheduled time, so exactly one replica executes
// it. Polling checks happen at slots aligned to their interval plus a fixed
// offset per trigger, so all replicas compute the same slots; each slot is
// requested once and the Checker runs it.
type Scheduler struct {
	store Store
	eng   Engine
	reg   *nodes.Registry
	log   *slog.Logger
	opts  SchedulerOptions
	now   func() time.Time

	checks *Checker

	mu      sync.Mutex
	entries map[string]*wfEntry // workflow id -> jobs
	healthy map[store.TriggerRef]bool
	wake    chan struct{}
}

type wfEntry struct {
	version int
	jobs    []*job
}

type job struct {
	workflowID string
	nodeID     string
	sched      *nodes.Schedule // schedule triggers
	poll       time.Duration   // polling triggers: configured interval
	push       bool            // polling triggers: push delivery is healthy
	next       time.Time
}

// interval is the time between checks of a polling job.
func (j *job) interval() time.Duration { return EffectiveInterval(j.poll, j.push) }

// nextSlot returns the first multiple of d after t (counted from the zero
// time), the same on every replica.
func nextSlot(t time.Time, d time.Duration) time.Time {
	return t.Truncate(d).Add(d)
}

func NewScheduler(st Store, eng Engine, reg *nodes.Registry, log *slog.Logger, opts SchedulerOptions) *Scheduler {
	if opts.CatchUp <= 0 {
		opts.CatchUp = time.Hour
	}
	if opts.Resync <= 0 {
		opts.Resync = 30 * time.Second
	}
	s := &Scheduler{
		store: st, eng: eng, reg: reg, log: log.With("component", "scheduler"), opts: opts,
		now: time.Now, entries: map[string]*wfEntry{}, healthy: map[store.TriggerRef]bool{}, wake: make(chan struct{}, 1),
	}
	s.checks = newChecker(st, st.GetWorkflow, eng, reg, s.log, opts.Instance, opts.CheckWorkers)
	return s
}

// Checks runs the polling checks; push sources wake triggers through it.
func (s *Scheduler) Checks() *Checker { return s.checks }

// Run blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	checksDone := make(chan struct{})
	go func() { defer close(checksDone); s.checks.Run(ctx) }()
	defer func() { <-checksDone }()
	s.sync(ctx)
	resync := time.NewTicker(s.opts.Resync)
	defer resync.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()

	for {
		s.fireDue(ctx)

		wait := s.untilNext()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-s.wake:
			timer.Stop()
		case <-resync.C:
			timer.Stop()
			s.sync(ctx)
		case <-prune.C:
			timer.Stop()
			if n, err := s.store.PruneFirings(ctx, 7*24*time.Hour); err != nil {
				s.log.Error("prune firings", "err", err)
			} else if n > 0 {
				s.log.Info("pruned firing records", "count", n)
			}
			if p, ok := s.store.(statePruner); ok {
				if n, err := p.PruneState(ctx); err != nil {
					s.log.Error("prune workflow state", "err", err)
				} else if n > 0 {
					s.log.Info("pruned expired workflow state", "count", n)
				}
			}
			s.pruneExecutions(ctx)
		}
	}
}

// WorkflowChanged implements api.ChangeListener.
func (s *Scheduler) WorkflowChanged(w *workflow.Workflow) {
	s.mu.Lock()
	s.setLocked(context.Background(), w)
	s.mu.Unlock()
	s.poke()
}

// WorkflowDeleted implements api.ChangeListener.
func (s *Scheduler) WorkflowDeleted(id string) {
	s.mu.Lock()
	delete(s.entries, id)
	s.mu.Unlock()
	s.poke()
}

func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// sync reloads all active workflows and rebuilds changed entries, and
// adapts poll intervals to the current push health.
func (s *Scheduler) sync(ctx context.Context) {
	wfs, err := s.store.ListActiveWorkflows(ctx)
	if err != nil {
		s.log.Error("load active workflows", "err", err)
		return
	}
	var healthy map[store.TriggerRef]bool
	if s.opts.Health != nil {
		refs, err := s.opts.Health.HealthyPushTriggers(ctx)
		if err != nil {
			s.log.Error("load push health", "err", err)
		} else {
			healthy = map[store.TriggerRef]bool{}
			for _, r := range refs {
				healthy[r] = true
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if healthy != nil {
		s.healthy = healthy
		now := s.now()
		for _, e := range s.entries {
			for _, j := range e.jobs {
				if push := healthy[store.TriggerRef{WorkflowID: j.workflowID, NodeID: j.nodeID}]; j.poll > 0 && push != j.push {
					j.push = push
					j.next = NextPoll(j.workflowID, j.nodeID, j.interval(), now)
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, w := range wfs {
		seen[w.ID] = true
		if e, ok := s.entries[w.ID]; ok && e.version == w.Version {
			continue
		}
		s.setLocked(ctx, w)
	}
	for id := range s.entries {
		if !seen[id] {
			delete(s.entries, id)
		}
	}
}

// setLocked (re)builds the jobs for w. Caller holds s.mu.
func (s *Scheduler) setLocked(ctx context.Context, w *workflow.Workflow) {
	delete(s.entries, w.ID)
	if !w.Active {
		return
	}
	now := s.now()
	entry := &wfEntry{version: w.Version}
	for _, n := range w.Graph.Nodes {
		if n.Disabled {
			continue
		}
		if s.reg.TriggerKind(n.Type) == nodes.TriggerPoll {
			j := &job{workflowID: w.ID, nodeID: n.ID, poll: nodes.PollInterval(n.Params)}
			j.push = s.healthy[store.TriggerRef{WorkflowID: w.ID, NodeID: n.ID}]
			j.next = NextPoll(w.ID, n.ID, j.interval(), now)
			entry.jobs = append(entry.jobs, j)
			continue
		}
		if s.reg.TriggerKind(n.Type) != nodes.TriggerSchedule {
			continue
		}
		sched, err := nodes.ParseSchedule(n.Params)
		if err != nil {
			s.log.Warn("invalid schedule", "workflow", w.ID, "node", n.ID, "err", err)
			continue
		}
		j := &job{workflowID: w.ID, nodeID: n.ID, sched: sched}
		if sched.Mode == nodes.ScheduleOnce {
			switch {
			case sched.At.After(now):
				j.next = sched.At
			case now.Sub(sched.At) <= s.opts.CatchUp:
				fired, err := s.store.HasFired(ctx, w.ID, n.ID, fireKey(sched, sched.At))
				if err != nil || fired {
					continue
				}
				j.next = sched.At // due now: catch up
			default:
				continue // too far in the past
			}
		} else {
			j.next = sched.Next(now)
		}
		entry.jobs = append(entry.jobs, j)
	}
	// Keep empty entries too, so resync can skip unchanged workflows cheaply.
	s.entries[w.ID] = entry
}

func (s *Scheduler) untilNext() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	wait := s.opts.Resync
	now := s.now()
	for _, e := range s.entries {
		for _, j := range e.jobs {
			if d := j.next.Sub(now); d < wait {
				wait = d
			}
		}
	}
	if wait < 0 {
		wait = 0
	}
	return wait
}

type firing struct {
	job         job
	scheduledAt time.Time
}

func (s *Scheduler) fireDue(ctx context.Context) {
	now := s.now()
	var due []firing
	s.mu.Lock()
	for _, e := range s.entries {
		kept := e.jobs[:0]
		for _, j := range e.jobs {
			if j.next.After(now) {
				kept = append(kept, j)
				continue
			}
			due = append(due, firing{job: *j, scheduledAt: j.next})
			if j.poll > 0 {
				j.next = NextPoll(j.workflowID, j.nodeID, j.interval(), now)
				kept = append(kept, j)
			} else if j.sched.Mode == nodes.ScheduleCron {
				j.next = j.sched.Next(now)
				kept = append(kept, j)
			}
		}
		e.jobs = kept
	}
	s.mu.Unlock()

	for _, f := range due {
		if f.job.poll > 0 {
			s.requestCheck(ctx, f)
			continue
		}
		go s.fire(ctx, f)
	}
}

// requestCheck asks for the check of a polling slot. The first replica to
// request the slot hands it to its Checker; the request is stored, so the
// check happens even if this replica stops before running it.
func (s *Scheduler) requestCheck(ctx context.Context, f firing) {
	ok, err := s.store.RequestSlot(ctx, f.job.workflowID, f.job.nodeID, f.scheduledAt)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("request polling check", "workflow", f.job.workflowID, "node", f.job.nodeID, "err", err)
		}
		return
	}
	if ok {
		s.checks.enqueue(store.TriggerRef{WorkflowID: f.job.workflowID, NodeID: f.job.nodeID})
	}
}

func fireKey(sched *nodes.Schedule, at time.Time) string {
	if sched.Mode == nodes.ScheduleOnce {
		return "once:" + at.UTC().Format(time.RFC3339)
	}
	return "cron:" + at.UTC().Format(time.RFC3339)
}

func (s *Scheduler) fire(ctx context.Context, f firing) {
	log := s.log.With("workflow", f.job.workflowID, "node", f.job.nodeID, "scheduledAt", f.scheduledAt)
	claimed, err := s.store.ClaimFiring(ctx, f.job.workflowID, f.job.nodeID, fireKey(f.job.sched, f.scheduledAt))
	if err != nil {
		log.Error("claim firing", "err", err)
		return
	}
	if !claimed {
		return // another replica (or an earlier run) handled it
	}
	// Reload so the run uses the latest saved graph.
	w, err := s.store.GetWorkflow(ctx, f.job.workflowID)
	if err != nil {
		log.Error("load workflow", "err", err)
		return
	}
	if !w.Active {
		return
	}
	exec, err := s.eng.Start(ctx, engine.Request{
		Workflow:    w,
		TriggerType: nodes.TriggerSchedule,
		TriggerNode: f.job.nodeID,
		TriggeredBy: "schedule",
		Items: []nodes.Item{{
			"firedAt":     s.now().UTC().Format(time.RFC3339),
			"scheduledAt": f.scheduledAt.In(f.job.sched.Location).Format(time.RFC3339),
			"timezone":    f.job.sched.Location.String(),
		}},
	})
	if err != nil {
		log.Error("start scheduled execution", "err", err)
		return
	}
	log.Info("schedule fired", "execution", exec.ID)
}

// pruneExecutions deletes finished executions past the retention period in
// batches, stopping after pruneMaxBatches (the next tick continues).
func (s *Scheduler) pruneExecutions(ctx context.Context) {
	p, ok := s.store.(executionPruner)
	if !ok || s.opts.ExecutionRetention <= 0 {
		return
	}
	var total int64
	for i := 0; i < pruneMaxBatches && ctx.Err() == nil; i++ {
		n, err := p.PruneExecutions(ctx, s.opts.ExecutionRetention, pruneBatch)
		total += n
		if err != nil {
			s.log.Error("prune executions", "err", err)
			break
		}
		if n < pruneBatch {
			break
		}
	}
	if total > 0 {
		s.log.Info("pruned old executions", "count", total)
	}
}
