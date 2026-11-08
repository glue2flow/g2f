package engine

// Recovery: executions survive the instance that runs them.
//
// Every instance has an id and sends heartbeats. An execution belongs to the
// instance that runs it (store.Execution.Owner) and is checkpointed after
// every node: the run snapshot (runState) and the steps so far. If an
// instance stops sending heartbeats (crash, OOM kill, network partition),
// another instance claims its executions and continues each one after its
// last completed node. On graceful shutdown, executions that do not finish
// within the grace period are handed back right away (ReleaseExecution).
//
// Guarantees:
//   - Completed nodes never run again.
//   - The node that was running when the instance stopped runs again
//     (at-least-once): its side effects may happen twice.
//   - Writes are fenced by owner, so an instance that was cut off and is
//     still running stops at its next checkpoint instead of finishing a run
//     that another instance took over.
//   - Sub-workflow executions are not continued: the parent re-runs its
//     Execute Workflow node, which starts a new one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/glue2flow/g2f/internal/store"
)

// RecoveryStore persists checkpoints and instance heartbeats. *store.Store
// implements it; without it, executions of a stopped instance are lost.
type RecoveryStore interface {
	Checkpoint(ctx context.Context, e *store.Execution, state []byte) error
	ReleaseExecution(ctx context.Context, e *store.Execution, state []byte) error
	ReturnToWaiting(ctx context.Context, e *store.Execution) error
	Heartbeat(ctx context.Context, instance, hostname string) error
	RemoveInstance(ctx context.Context, instance string) error
	ClaimOrphans(ctx context.Context, instance string, stale, legacyAge time.Duration, limit int) ([]store.Orphan, error)
}

// MaxRecoveries is how often an execution is taken over before it fails.
// Protects against runs that crash every instance (e.g. by running it out of memory).
const MaxRecoveries = 3

// errTakenOver stops a run whose execution now belongs to another instance.
var errTakenOver = errors.New("execution was taken over by another instance")

// InstanceID identifies this engine in the instance registry.
func (e *Engine) InstanceID() string { return e.instance }

// Register announces the instance. Call it once before serving requests.
func (e *Engine) Register(ctx context.Context) error {
	if e.rs == nil {
		return nil
	}
	return e.rs.Heartbeat(ctx, e.instance, hostname())
}

// RunRecovery sends heartbeats and takes over executions of stopped
// instances until ctx is done. stale is how long an instance may be silent.
func (e *Engine) RunRecovery(ctx context.Context, stale time.Duration) {
	if e.rs == nil {
		return
	}
	every := max(stale/3, time.Second)
	t := time.NewTicker(every)
	defer t.Stop()
	host := hostname()
	for {
		if err := e.rs.Heartbeat(ctx, e.instance, host); err != nil && ctx.Err() == nil {
			e.log.Error("instance heartbeat", "err", err)
		}
		if !e.draining.Load() {
			e.recoverOrphans(ctx, stale)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (e *Engine) recoverOrphans(ctx context.Context, stale time.Duration) {
	// Only take what the worker pool can start soon.
	free := cap(e.sem) - len(e.sem)
	if free <= 0 {
		return
	}
	orphans, err := e.rs.ClaimOrphans(ctx, e.instance, stale, e.opts.Timeout, free)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Error("claim interrupted executions", "err", err)
		}
		return
	}
	for _, o := range orphans {
		e.recover(o)
	}
}

// recover continues (or ends) one claimed execution.
func (e *Engine) recover(o store.Orphan) {
	exec := o.Execution
	log := e.log.With("execution", exec.ID, "attempt", exec.Attempts)
	if o.State == nil {
		log.Warn("interrupted execution has no checkpoint")
		e.finish(exec, nil, nil, errors.New("interrupted: the instance running it stopped before it saved any progress"))
		return
	}
	var st runState
	if err := json.Unmarshal(o.State, &st); err != nil {
		e.finish(exec, nil, nil, fmt.Errorf("interrupted, and its checkpoint is corrupt: %w", err))
		return
	}
	switch {
	case st.WaitNode != "":
		// Resumed from a Wait node, stopped before saving progress: wait again.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := e.rs.ReturnToWaiting(ctx, exec); err != nil {
			log.Error("return execution to waiting", "err", err)
		}
		log.Info("interrupted execution is waiting again")
	case st.Depth > 0:
		e.finish(exec, &st, nil, errors.New("interrupted: the instance running it stopped; the calling workflow runs it again"))
	case exec.Attempts > MaxRecoveries:
		e.finish(exec, &st, nil, fmt.Errorf("interrupted %d times; not continued again", exec.Attempts))
	default:
		log.Info("continuing interrupted execution")
		st.recovered = true
		e.background(exec, &st, nil)
	}
}

// checkpoint saves the progress of a run.
func (e *Engine) checkpoint(exec *store.Execution, st *runState) error {
	if e.rs == nil || exec.Owner == "" {
		return nil
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.rs.Checkpoint(ctx, exec, data); err != nil {
		if errors.Is(err, store.ErrNotOwner) {
			return errTakenOver
		}
		// The run goes on; only its recoverability suffers.
		e.log.Error("checkpoint execution", "id", exec.ID, "err", err)
	}
	return nil
}

// shouldRelease reports whether a run interrupted with ctx should be handed
// to another instance instead of failing: the engine is shutting down.
func (e *Engine) shouldRelease(ctx context.Context, st *runState) bool {
	return e.rs != nil && st.Depth == 0 && e.baseCtx.Err() != nil && ctx.Err() != nil
}

// release hands an unfinished run back so another instance continues it.
// st must describe the state before the interrupted node.
func (e *Engine) release(exec *store.Execution, st *runState) *Result {
	data, err := json.Marshal(st)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = e.rs.ReleaseExecution(ctx, exec, data)
	}
	if err != nil {
		e.log.Error("release execution", "id", exec.ID, "err", err)
	} else {
		e.log.Info("released unfinished execution", "id", exec.ID)
	}
	exec.Status = store.StatusQueued
	return &Result{Execution: exec}
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
