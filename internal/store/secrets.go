package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/glue2flow/g2f/internal/workflow"
)

// SecretOp changes one encrypted node parameter.
type SecretOp struct {
	NodeID string
	Param  string
	Sealed string // encrypted value; ignored when Delete
	Delete bool
}

func applySecretOps(ctx context.Context, tx pgx.Tx, w *workflow.Workflow, ops []SecretOp) error {
	for _, op := range ops {
		var err error
		if op.Delete {
			_, err = tx.Exec(ctx, `DELETE FROM node_secrets WHERE workflow_id=$1 AND node_id=$2 AND param=$3`,
				w.ID, op.NodeID, op.Param)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO node_secrets (workflow_id, node_id, param, value)
				VALUES ($1,$2,$3,$4)
				ON CONFLICT (workflow_id, node_id, param) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`,
				w.ID, op.NodeID, op.Param, op.Sealed)
		}
		if err != nil {
			return err
		}
	}
	ids := make([]string, len(w.Graph.Nodes))
	for i, n := range w.Graph.Nodes {
		ids[i] = n.ID
	}
	_, err := tx.Exec(ctx, `DELETE FROM node_secrets WHERE workflow_id=$1 AND NOT (node_id = ANY($2))`, w.ID, ids)
	return err
}

// NodeSecrets returns the sealed secrets of a workflow: node id -> param -> sealed value.
func (s *Store) NodeSecrets(ctx context.Context, workflowID string) (map[string]map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT node_id, param, value FROM node_secrets WHERE workflow_id=$1`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var node, param, value string
		if err := rows.Scan(&node, &param, &value); err != nil {
			return nil, err
		}
		if out[node] == nil {
			out[node] = map[string]string{}
		}
		out[node][param] = value
	}
	return out, rows.Err()
}

// PluginConfig returns the sealed config values of a module for an organization.
func (s *Store) PluginConfig(ctx context.Context, org, module string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM plugin_config WHERE org_id=$1 AND module=$2`, org, module)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetPluginConfig upserts sealed values and deletes keys in del.
func (s *Store) SetPluginConfig(ctx context.Context, org, module, by string, set map[string]string, del []string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for k, v := range set {
			if _, err := tx.Exec(ctx, `INSERT INTO plugin_config (org_id, module, key, value, updated_by)
				VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (org_id, module, key) DO UPDATE SET value=EXCLUDED.value,
					updated_by=EXCLUDED.updated_by, updated_at=now()`, org, module, k, v, by); err != nil {
				return err
			}
		}
		if len(del) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM plugin_config WHERE org_id=$1 AND module=$2 AND key = ANY($3)`,
				org, module, del); err != nil {
				return err
			}
		}
		return nil
	})
}

// PluginRecord is an imported plugin.
type PluginRecord struct {
	Module      string            `json:"module"`
	Version     string            `json:"version"`
	Manifest    []byte            `json:"-"`
	Wasm        []byte            `json:"-"`
	Icons       map[string]string `json:"-"` // icon files: path -> data URI
	SHA256      string            `json:"sha256"`
	InstalledBy string            `json:"installedBy"`
	InstalledAt time.Time         `json:"installedAt"`
}

func (s *Store) ListPlugins(ctx context.Context) ([]*PluginRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT module, version, manifest, wasm, icons, sha256, installed_by, installed_at
		FROM plugins ORDER BY module`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PluginRecord
	for rows.Next() {
		var p PluginRecord
		var manifest string
		if err := rows.Scan(&p.Module, &p.Version, &manifest, &p.Wasm, &p.Icons, &p.SHA256, &p.InstalledBy, &p.InstalledAt); err != nil {
			return nil, err
		}
		p.Manifest = []byte(manifest)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// PluginDigests returns module -> sha256 for cheap change detection.
func (s *Store) PluginDigests(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT module, sha256 FROM plugins`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var m, d string
		if err := rows.Scan(&m, &d); err != nil {
			return nil, err
		}
		out[m] = d
	}
	return out, rows.Err()
}

func (s *Store) SavePlugin(ctx context.Context, p *PluginRecord) error {
	icons := p.Icons
	if icons == nil {
		icons = map[string]string{}
	}
	return s.pool.QueryRow(ctx, `INSERT INTO plugins (module, version, manifest, wasm, icons, sha256, installed_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (module) DO UPDATE SET version=EXCLUDED.version, manifest=EXCLUDED.manifest,
			wasm=EXCLUDED.wasm, icons=EXCLUDED.icons, sha256=EXCLUDED.sha256, installed_by=EXCLUDED.installed_by, installed_at=now()
		RETURNING installed_at`,
		p.Module, p.Version, string(p.Manifest), p.Wasm, icons, p.SHA256, p.InstalledBy).Scan(&p.InstalledAt)
}

func (s *Store) DeletePlugin(ctx context.Context, module string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM plugins WHERE module=$1`, module)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
