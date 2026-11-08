package nodes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// StateStore is durable key/value storage of one workflow, kept across runs
// (PostgreSQL in production). Values are grouped in scopes, e.g. "data" for
// the Data Store node or "dedupe:<name>" for Remove Duplicates.
type StateStore interface {
	Get(ctx context.Context, scope, key string) (any, bool, error)
	// Set stores a JSON value; ttl <= 0 keeps it until it is deleted.
	Set(ctx context.Context, scope, key string, value any, ttl time.Duration) error
	Delete(ctx context.Context, scope, key string) (bool, error)
	Clear(ctx context.Context, scope string) (int64, error)
	// Increment atomically adds by to a number (missing: 0) and returns it.
	Increment(ctx context.Context, scope, key string, by float64, ttl time.Duration) (float64, error)
	List(ctx context.Context, scope, prefix string, limit int) ([]StateEntry, error)
	// Remember records keys and reports for each whether it was new. Parallel
	// runs never both see the same key as new.
	Remember(ctx context.Context, scope string, keys []string, ttl time.Duration) ([]bool, error)
	// Seen reports for each key whether it is recorded, without recording.
	Seen(ctx context.Context, scope string, keys []string) ([]bool, error)
}

// StateEntry is one stored value.
type StateEntry struct {
	Key       string    `json:"key"`
	Value     any       `json:"value"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ErrNoState is returned by nodes that need a state store when none is configured.
var ErrNoState = errors.New("durable workflow state is not available")

// StateKey hashes a value into a fixed-size key, for keys derived from data
// (message IDs, URLs, whole items).
func StateKey(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:16])
}

// IsTestRun reports whether the node runs in a manual run from the editor.
// Nodes with lasting effects on state (Remove Duplicates, polling triggers)
// do not record anything then.
func (c *ExecContext) IsTestRun() bool { return c.TriggerType == string(TriggerManual) }

// --- in-memory implementation (tests, single-process tools) ----------------

// MemoryState is a StateStore kept in memory.
type MemoryState struct {
	mu   sync.Mutex
	data map[string]map[string]memEntry
	now  func() time.Time
}

type memEntry struct {
	value   any
	updated time.Time
	expires time.Time
}

func NewMemoryState() *MemoryState {
	return &MemoryState{data: map[string]map[string]memEntry{}, now: time.Now}
}

func (m *MemoryState) live(scope, key string) (memEntry, bool) {
	e, ok := m.data[scope][key]
	if ok && !e.expires.IsZero() && !e.expires.After(m.now()) {
		return e, false
	}
	return e, ok
}

func (m *MemoryState) put(scope, key string, v any, ttl time.Duration) {
	if m.data[scope] == nil {
		m.data[scope] = map[string]memEntry{}
	}
	e := memEntry{value: v, updated: m.now()}
	if ttl > 0 {
		e.expires = m.now().Add(ttl)
	}
	m.data[scope][key] = e
}

func (m *MemoryState) Get(_ context.Context, scope, key string) (any, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live(scope, key)
	return e.value, ok, nil
}

// Set stores value as JSON would (structs become maps, numbers float64),
// like the database does.
func (m *MemoryState) Set(_ context.Context, scope, key string, value any, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.put(scope, key, v, ttl)
	return nil
}

func (m *MemoryState) Delete(_ context.Context, scope, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[scope][key]
	delete(m.data[scope], key)
	return ok, nil
}

func (m *MemoryState) Clear(_ context.Context, scope string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.data[scope])
	delete(m.data, scope)
	return int64(n), nil
}

func (m *MemoryState) Increment(_ context.Context, scope, key string, by float64, ttl time.Duration) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := 0.0
	e, ok := m.live(scope, key)
	if ok {
		switch v := e.value.(type) {
		case float64:
			cur = v
		case int:
			cur = float64(v)
		case nil:
		default:
			return 0, errors.New("stored value is not a number")
		}
		if ttl <= 0 && !e.expires.IsZero() {
			ttl = e.expires.Sub(m.now())
		}
	}
	m.put(scope, key, cur+by, ttl)
	return cur + by, nil
}

func (m *MemoryState) List(_ context.Context, scope, prefix string, limit int) ([]StateEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []StateEntry
	for k := range m.data[scope] {
		if e, ok := m.live(scope, k); ok && strings.HasPrefix(k, prefix) {
			out = append(out, StateEntry{Key: k, Value: e.value, UpdatedAt: e.updated})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryState) Remember(_ context.Context, scope string, keys []string, ttl time.Duration) ([]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]bool, len(keys))
	for i, k := range keys {
		if _, ok := m.live(scope, k); !ok {
			out[i] = true
			m.put(scope, k, nil, ttl)
		}
	}
	return out, nil
}

func (m *MemoryState) Seen(_ context.Context, scope string, keys []string) ([]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]bool, len(keys))
	for i, k := range keys {
		_, out[i] = m.live(scope, k)
	}
	return out, nil
}
