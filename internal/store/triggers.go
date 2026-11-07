package store

import (
	"context"
	"time"
)

// ClaimFiring records that a trigger fired for key. It returns false if the
// firing was already claimed (by this or another instance).
func (s *Store) ClaimFiring(ctx context.Context, workflowID, nodeID, key string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO trigger_firings (workflow_id, node_id, fire_key)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, workflowID, nodeID, key)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// HasFired reports whether a firing for key exists.
func (s *Store) HasFired(ctx context.Context, workflowID, nodeID, key string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM trigger_firings
		WHERE workflow_id=$1 AND node_id=$2 AND fire_key=$3)`, workflowID, nodeID, key).Scan(&ok)
	return ok, err
}

// PruneFirings deletes recurring-schedule firing records older than age.
// One-time firings ("once:" keys) are kept so they never fire twice.
func (s *Store) PruneFirings(ctx context.Context, age time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM trigger_firings
		WHERE fired_at < $1 AND fire_key NOT LIKE 'once:%'`, time.Now().Add(-age))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
