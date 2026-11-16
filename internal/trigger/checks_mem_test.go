package trigger

import (
	"context"
	"sync"
	"time"

	"github.com/glue2flow/g2f/internal/store"
)

// memChecks implements CheckStore in memory with the semantics of the SQL
// in store/push.go.
type memChecks struct {
	mu   sync.Mutex
	rows map[store.TriggerRef]*memCheck
}

type memCheck struct {
	requested, started, lastSlot, leaseUntil time.Time
	owner                                    string
}

func (m *memChecks) row(wf, node string) *memCheck {
	if m.rows == nil {
		m.rows = map[store.TriggerRef]*memCheck{}
	}
	ref := store.TriggerRef{WorkflowID: wf, NodeID: node}
	r := m.rows[ref]
	if r == nil {
		r = &memCheck{}
		m.rows[ref] = r
	}
	return r
}

func (m *memChecks) RequestCheck(_ context.Context, wf, node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.row(wf, node).requested = time.Now()
	return nil
}

func (m *memChecks) RequestSlot(_ context.Context, wf, node string, slot time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.row(wf, node)
	if !r.lastSlot.IsZero() && !r.lastSlot.Before(slot) {
		return false, nil
	}
	r.lastSlot, r.requested = slot, time.Now()
	return true, nil
}

func (m *memChecks) AcquireCheck(_ context.Context, wf, node, owner string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.row(wf, node)
	now := time.Now()
	if r.requested.IsZero() || (r.owner != "" && r.leaseUntil.After(now)) {
		return false, nil
	}
	r.owner, r.leaseUntil, r.started = owner, now.Add(ttl), now
	return true, nil
}

func (m *memChecks) ReleaseCheck(_ context.Context, wf, node, owner string, done bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.row(wf, node)
	if r.owner != owner {
		return false, nil
	}
	r.owner = ""
	if done && !r.requested.After(r.started) {
		r.requested = time.Time{}
	}
	return !r.requested.IsZero(), nil
}

func (m *memChecks) RenewChecks(_ context.Context, owner string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.owner == owner {
			r.leaseUntil = time.Now().Add(ttl)
		}
	}
	return nil
}

func (m *memChecks) PendingChecks(_ context.Context, limit int) ([]store.TriggerRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.TriggerRef
	for ref, r := range m.rows {
		if !r.requested.IsZero() && (r.owner == "" || r.leaseUntil.Before(time.Now())) && len(out) < limit {
			out = append(out, ref)
		}
	}
	return out, nil
}
