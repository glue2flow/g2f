package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// StateEntry is one stored value of a workflow.
type StateEntry struct {
	Key       string
	Value     json.RawMessage
	UpdatedAt time.Time
	ExpiresAt *time.Time
}

func expiry(ttl time.Duration) *time.Time {
	if ttl <= 0 {
		return nil
	}
	t := time.Now().Add(ttl).UTC()
	return &t
}

const notExpired = `(expires_at IS NULL OR expires_at > now())`

// StateGet returns a value, or found=false if it is missing or expired.
func (s *Store) StateGet(ctx context.Context, workflowID, scope, key string) (json.RawMessage, bool, error) {
	var v json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT value FROM workflow_state
		WHERE workflow_id=$1 AND scope=$2 AND key=$3 AND `+notExpired, workflowID, scope, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return v, err == nil, err
}

// StateSet stores a value (ttl <= 0: no expiry).
func (s *Store) StateSet(ctx context.Context, workflowID, scope, key string, value json.RawMessage, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO workflow_state (workflow_id, scope, key, value, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workflow_id, scope, key) DO UPDATE
		SET value=EXCLUDED.value, expires_at=EXCLUDED.expires_at, updated_at=now()`,
		workflowID, scope, key, value, expiry(ttl))
	return err
}

// StateDelete removes a value and reports whether it existed.
func (s *Store) StateDelete(ctx context.Context, workflowID, scope, key string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM workflow_state WHERE workflow_id=$1 AND scope=$2 AND key=$3`, workflowID, scope, key)
	return tag.RowsAffected() > 0, err
}

// StateClear removes all values of a scope.
func (s *Store) StateClear(ctx context.Context, workflowID, scope string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM workflow_state WHERE workflow_id=$1 AND scope=$2`, workflowID, scope)
	return tag.RowsAffected(), err
}

// StateIncrement atomically adds by to a numeric value (missing or expired: 0)
// and returns the new value.
func (s *Store) StateIncrement(ctx context.Context, workflowID, scope, key string, by float64, ttl time.Duration) (float64, error) {
	var v float64
	err := s.pool.QueryRow(ctx, `INSERT INTO workflow_state (workflow_id, scope, key, value, expires_at)
		VALUES ($1, $2, $3, to_jsonb($4::float8), $5)
		ON CONFLICT (workflow_id, scope, key) DO UPDATE
		SET value = to_jsonb(CASE WHEN workflow_state.expires_at IS NOT NULL AND workflow_state.expires_at <= now() THEN 0
		                          ELSE COALESCE((workflow_state.value #>> '{}')::float8, 0) END + $4::float8),
		    expires_at = CASE WHEN workflow_state.expires_at IS NOT NULL AND workflow_state.expires_at <= now() THEN EXCLUDED.expires_at
		                      ELSE COALESCE(EXCLUDED.expires_at, workflow_state.expires_at) END,
		    updated_at = now()
		RETURNING (value #>> '{}')::float8`,
		workflowID, scope, key, by, expiry(ttl)).Scan(&v)
	return v, err
}

// StateList returns the values of a scope whose keys start with prefix, by key.
func (s *Store) StateList(ctx context.Context, workflowID, scope, prefix string, limit int) ([]StateEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value, updated_at, expires_at FROM workflow_state
		WHERE workflow_id=$1 AND scope=$2 AND starts_with(key, $3) AND `+notExpired+`
		ORDER BY key LIMIT $4`, workflowID, scope, prefix, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateEntry
	for rows.Next() {
		var e StateEntry
		if err := rows.Scan(&e.Key, &e.Value, &e.UpdatedAt, &e.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// StateRemember records keys and reports, per key, whether it was new (not
// stored, or expired). It is atomic, so parallel runs never both see a key as new.
func (s *Store) StateRemember(ctx context.Context, workflowID, scope string, keys []string, ttl time.Duration) ([]bool, error) {
	rows, err := s.pool.Query(ctx, `INSERT INTO workflow_state (workflow_id, scope, key, expires_at)
		SELECT $1, $2, k, $4 FROM unnest($3::text[]) AS k
		ON CONFLICT (workflow_id, scope, key) DO UPDATE
		SET expires_at = EXCLUDED.expires_at, updated_at = now()
		WHERE workflow_state.expires_at IS NOT NULL AND workflow_state.expires_at <= now()
		RETURNING key`, workflowID, scope, keys, expiry(ttl))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fresh := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		fresh[k] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]bool, len(keys))
	for i, k := range keys {
		out[i] = fresh[k]
	}
	return out, nil
}

// StateSeen reports, per key, whether it is stored and not expired.
func (s *Store) StateSeen(ctx context.Context, workflowID, scope string, keys []string) ([]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT key FROM workflow_state
		WHERE workflow_id=$1 AND scope=$2 AND key = ANY($3::text[]) AND `+notExpired, workflowID, scope, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		seen[k] = true
	}
	out := make([]bool, len(keys))
	for i, k := range keys {
		out[i] = seen[k]
	}
	return out, rows.Err()
}

// PruneState deletes expired values.
func (s *Store) PruneState(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM workflow_state WHERE expires_at IS NOT NULL AND expires_at <= now()`)
	return tag.RowsAffected(), err
}
