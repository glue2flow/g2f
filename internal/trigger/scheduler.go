// Package trigger starts workflows from schedules, polling triggers and webhooks.
package trigger

import (
	"context"
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

type SchedulerOptions struct {
	// CatchUp: a one-time schedule missed (e.g. during downtime) by at most this
	// long still fires once the server is back. Recurring schedules never catch up.
	CatchUp time.Duration
	// Resync: how often the active workflow set is reloaded from the database,
	// which picks up changes made through other replicas.
	Resync time.Duration
}

// Scheduler fires schedule triggers and runs the checks of polling triggers
// of active workflows.
//
// Every replica may run a Scheduler: each firing is claimed in the database
// with a key derived from the scheduled time, so exactly one replica executes
// it. Polling checks happen at times aligned to their interval (e.g. every
// full 5 minutes), so all replicas compute the same keys.
type Scheduler struct {
	store Store
	eng   Engine
	reg   *nodes.Registry
	log   *slog.Logger
	opts  SchedulerOptions
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]*wfEntry // workflow id -> jobs
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
	poll       time.Duration   // polling triggers: interval
	next       time.Time
}

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
	return &Scheduler{
		store: st, eng: eng, reg: reg, log: log.With("component", "scheduler"), opts: opts,
		now: time.Now, entries: map[string]*wfEntry{}, wake: make(chan struct{}, 1),
	}
}

// Run blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
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

// sync reloads all active workflows and rebuilds changed entries.
func (s *Scheduler) sync(ctx context.Context) {
	wfs, err := s.store.ListActiveWorkflows(ctx)
	if err != nil {
		s.log.Error("load active workflows", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
			d := nodes.PollInterval(n.Params)
			entry.jobs = append(entry.jobs, &job{workflowID: w.ID, nodeID: n.ID, poll: d, next: nextSlot(now, d)})
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
				j.next = nextSlot(now, j.poll)
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
		go s.fire(ctx, f)
	}
}

func fireKey(sched *nodes.Schedule, at time.Time) string {
	if sched == nil {
		return "poll:" + at.UTC().Format(time.RFC3339)
	}
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
	if f.job.poll > 0 {
		s.check(ctx, w, f.job.nodeID, log)
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

// check runs one polling check and starts a run with the new items.
func (s *Scheduler) check(ctx context.Context, w *workflow.Workflow, nodeID string, log *slog.Logger) {
	if n := w.Graph.Node(nodeID); n == nil || n.Disabled || s.reg.TriggerKind(n.Type) != nodes.TriggerPoll {
		return // changed since the job was planned
	}
	items, err := s.eng.Poll(ctx, w, nodeID)
	if err != nil {
		log.Warn("polling check failed", "err", err)
		return
	}
	if len(items) == 0 {
		return
	}
	exec, err := s.eng.Start(ctx, engine.Request{
		Workflow: w, TriggerType: nodes.TriggerPoll, TriggerNode: nodeID, TriggeredBy: "poll", Items: items,
	})
	if err != nil {
		log.Error("start polled execution", "err", err)
		return
	}
	log.Info("polling trigger found new items", "items", len(items), "execution", exec.ID)
}
