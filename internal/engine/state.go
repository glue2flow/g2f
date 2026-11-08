package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
)

// StateBackend persists workflow state (implemented by *store.Store).
type StateBackend interface {
	StateGet(ctx context.Context, workflowID, scope, key string) (json.RawMessage, bool, error)
	StateSet(ctx context.Context, workflowID, scope, key string, value json.RawMessage, ttl time.Duration) error
	StateDelete(ctx context.Context, workflowID, scope, key string) (bool, error)
	StateClear(ctx context.Context, workflowID, scope string) (int64, error)
	StateIncrement(ctx context.Context, workflowID, scope, key string, by float64, ttl time.Duration) (float64, error)
	StateList(ctx context.Context, workflowID, scope, prefix string, limit int) ([]store.StateEntry, error)
	StateRemember(ctx context.Context, workflowID, scope string, keys []string, ttl time.Duration) ([]bool, error)
	StateSeen(ctx context.Context, workflowID, scope string, keys []string) ([]bool, error)
}

// Limits protect the database from runaway workflows.
const (
	maxStateValue = 256 << 10
	maxStateKey   = 512
	maxStateKeys  = 10000 // per Remember/Seen call
)

// workflowState binds a StateBackend to one workflow.
type workflowState struct {
	b  StateBackend
	id string
}

func checkKey(key string) error {
	if key == "" {
		return fmt.Errorf("state key must not be empty")
	}
	if len(key) > maxStateKey {
		return fmt.Errorf("state key longer than %d bytes", maxStateKey)
	}
	return nil
}

func checkKeys(keys []string) error {
	if len(keys) > maxStateKeys {
		return fmt.Errorf("more than %d keys at once", maxStateKeys)
	}
	for _, k := range keys {
		if err := checkKey(k); err != nil {
			return err
		}
	}
	return nil
}

func (w workflowState) Get(ctx context.Context, scope, key string) (any, bool, error) {
	raw, ok, err := w.b.StateGet(ctx, w.id, scope, key)
	if err != nil || !ok {
		return nil, ok, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func (w workflowState) Set(ctx context.Context, scope, key string, value any, ttl time.Duration) error {
	if err := checkKey(key); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("value is not JSON: %w", err)
	}
	if len(raw) > maxStateValue {
		return fmt.Errorf("value larger than %d KiB", maxStateValue>>10)
	}
	return w.b.StateSet(ctx, w.id, scope, key, raw, ttl)
}

func (w workflowState) Delete(ctx context.Context, scope, key string) (bool, error) {
	return w.b.StateDelete(ctx, w.id, scope, key)
}

func (w workflowState) Clear(ctx context.Context, scope string) (int64, error) {
	return w.b.StateClear(ctx, w.id, scope)
}

func (w workflowState) Increment(ctx context.Context, scope, key string, by float64, ttl time.Duration) (float64, error) {
	if err := checkKey(key); err != nil {
		return 0, err
	}
	v, err := w.b.StateIncrement(ctx, w.id, scope, key, by, ttl)
	if err != nil {
		return 0, fmt.Errorf("increment %q (is the stored value a number?): %w", key, err)
	}
	return v, nil
}

func (w workflowState) List(ctx context.Context, scope, prefix string, limit int) ([]nodes.StateEntry, error) {
	rows, err := w.b.StateList(ctx, w.id, scope, prefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]nodes.StateEntry, len(rows))
	for i, r := range rows {
		var v any
		_ = json.Unmarshal(r.Value, &v)
		out[i] = nodes.StateEntry{Key: r.Key, Value: v, UpdatedAt: r.UpdatedAt}
	}
	return out, nil
}

func (w workflowState) Remember(ctx context.Context, scope string, keys []string, ttl time.Duration) ([]bool, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if err := checkKeys(keys); err != nil {
		return nil, err
	}
	// A key may appear once per statement; repeats are never new.
	unique := make([]string, 0, len(keys))
	index := map[string]int{}
	for _, k := range keys {
		if _, ok := index[k]; !ok {
			index[k] = len(unique)
			unique = append(unique, k)
		}
	}
	res, err := w.b.StateRemember(ctx, w.id, scope, unique, ttl)
	if err != nil {
		return nil, err
	}
	out := make([]bool, len(keys))
	used := map[string]bool{}
	for i, k := range keys {
		out[i] = res[index[k]] && !used[k]
		used[k] = true
	}
	return out, nil
}

func (w workflowState) Seen(ctx context.Context, scope string, keys []string) ([]bool, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if err := checkKeys(keys); err != nil {
		return nil, err
	}
	return w.b.StateSeen(ctx, w.id, scope, keys)
}
