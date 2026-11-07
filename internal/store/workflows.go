package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/wolfposd/g8g/internal/auth"
	"github.com/wolfposd/g8g/internal/workflow"
)

const workflowCols = `id, org_id, name, description, owner_sub, owner_name, scope, org_role,
	shares, graph, active, version, created_at, updated_at`

func scanWorkflow(row pgx.Row) (*workflow.Workflow, error) {
	var w workflow.Workflow
	var id uuid.UUID
	var shares, graph []byte
	err := row.Scan(&id, &w.OrgID, &w.Name, &w.Description, &w.OwnerSub, &w.OwnerName,
		&w.Scope, &w.OrgRole, &shares, &graph, &w.Active, &w.Version, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w.ID = id.String()
	if err := json.Unmarshal(shares, &w.Shares); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(graph, &w.Graph); err != nil {
		return nil, err
	}
	return &w, nil
}

// ListVisibleWorkflows returns workflows u can at least view. The query mirrors
// workflow.RoleFor; callers still compute the exact role per workflow.
func (s *Store) ListVisibleWorkflows(ctx context.Context, u *auth.User) ([]*workflow.Workflow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+workflowCols+` FROM workflows w
		WHERE w.org_id = $1 AND (
			$4 OR w.owner_sub = $2 OR w.scope = 'org'
			OR (w.scope = 'group' AND EXISTS (
				SELECT 1 FROM jsonb_array_elements(w.shares) s WHERE s->>'group' = ANY($3)))
		)
		ORDER BY w.updated_at DESC`, u.Org, u.Sub, u.GroupPaths(), u.Admin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*workflow.Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) GetWorkflow(ctx context.Context, id string) (*workflow.Workflow, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrNotFound
	}
	return scanWorkflow(s.pool.QueryRow(ctx, `SELECT `+workflowCols+` FROM workflows WHERE id=$1`, uid))
}

// ListActiveWorkflows returns all active workflows across all orgs (used by triggers).
func (s *Store) ListActiveWorkflows(ctx context.Context) ([]*workflow.Workflow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+workflowCols+` FROM workflows WHERE active`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*workflow.Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) CreateWorkflow(ctx context.Context, w *workflow.Workflow) error {
	w.ID = uuid.NewString()
	w.Version = 1
	shares, graph, err := marshalWF(w)
	if err != nil {
		return err
	}
	return s.pool.QueryRow(ctx, `INSERT INTO workflows
		(id, org_id, name, description, owner_sub, owner_name, scope, org_role, shares, graph, active, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING created_at, updated_at`,
		w.ID, w.OrgID, w.Name, w.Description, w.OwnerSub, w.OwnerName, w.Scope, w.OrgRole,
		shares, graph, w.Active, w.Version,
	).Scan(&w.CreatedAt, &w.UpdatedAt)
}

// UpdateWorkflow saves w if its Version matches the stored one and bumps the version.
func (s *Store) UpdateWorkflow(ctx context.Context, w *workflow.Workflow) error {
	shares, graph, err := marshalWF(w)
	if err != nil {
		return err
	}
	err = s.pool.QueryRow(ctx, `UPDATE workflows SET
		name=$3, description=$4, scope=$5, org_role=$6, shares=$7, graph=$8, active=$9,
		version=version+1, updated_at=now()
		WHERE id=$1 AND version=$2
		RETURNING version, updated_at`,
		w.ID, w.Version, w.Name, w.Description, w.Scope, w.OrgRole, shares, graph, w.Active,
	).Scan(&w.Version, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	return err
}

func (s *Store) DeleteWorkflow(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM workflows WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func marshalWF(w *workflow.Workflow) (shares, graph []byte, err error) {
	if shares, err = json.Marshal(w.Shares); err != nil {
		return
	}
	graph, err = json.Marshal(w.Graph)
	return
}
