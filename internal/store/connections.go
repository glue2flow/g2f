package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/glue2flow/g2f/internal/auth"
)

// Connection kinds.
const (
	KindOAuth2     = "oauth2"
	KindCredential = "credential"
)

// Connection is a connected OAuth2 account or an API-key credential.
// AccessToken, RefreshToken and SecretFields hold sealed values and are never
// serialized.
type Connection struct {
	ID           string     `json:"id"`
	OrgID        string     `json:"orgId"`
	Module       string     `json:"module"`
	Kind         string     `json:"kind"` // oauth2 | credential
	Name         string     `json:"name"`
	Account      string     `json:"account"`
	OwnerSub     string     `json:"ownerSub"`
	OwnerName    string     `json:"ownerName"`
	Scope        string     `json:"scope"` // private | group | org
	Groups       []string   `json:"groups"`
	AccessToken  string     `json:"-"`
	RefreshToken string     `json:"-"`
	TokenType    string     `json:"tokenType"`
	Scopes       string     `json:"scopes"`
	InstanceURL  string     `json:"instanceUrl,omitempty"` // API base URL of the account, if the provider has one
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	// Fields are the non-secret fields of a credential; SecretFields its
	// secret fields, sealed with secrets.CredentialAAD.
	Fields       map[string]string `json:"fields,omitempty"`
	SecretFields map[string]string `json:"-"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
}

// UsableBy reports whether u may select the connection in workflows.
func (c *Connection) UsableBy(u *auth.User) bool {
	if u == nil || u.Org != c.OrgID {
		return false
	}
	if c.ManageableBy(u) || c.Scope == "org" {
		return true
	}
	if c.Scope == "group" {
		for _, g := range c.Groups {
			if u.InGroup(g) {
				return true
			}
		}
	}
	return false
}

// ManageableBy reports whether u may rename, share or delete the connection.
func (c *Connection) ManageableBy(u *auth.User) bool {
	return u != nil && u.Org == c.OrgID && (u.Sub == c.OwnerSub || u.Admin)
}

const connCols = `id, org_id, module, name, account, owner_sub, owner_name, scope, groups,
	access_token, refresh_token, token_type, scopes, expires_at, created_at, updated_at, instance_url,
	kind, fields, secret_fields`

func scanConnection(row pgx.Row) (*Connection, error) {
	var c Connection
	var id uuid.UUID
	var groups, fields, secretFields []byte
	var instanceURL *string
	err := row.Scan(&id, &c.OrgID, &c.Module, &c.Name, &c.Account, &c.OwnerSub, &c.OwnerName, &c.Scope, &groups,
		&c.AccessToken, &c.RefreshToken, &c.TokenType, &c.Scopes, &c.ExpiresAt, &c.CreatedAt, &c.UpdatedAt, &instanceURL,
		&c.Kind, &fields, &secretFields)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.ID = id.String()
	if instanceURL != nil {
		c.InstanceURL = *instanceURL
	}
	if err := json.Unmarshal(groups, &c.Groups); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(fields, &c.Fields); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(secretFields, &c.SecretFields); err != nil {
		return nil, err
	}
	return &c, nil
}

// fieldsJSON encodes credential field maps; nil becomes {}.
func fieldsJSON(m map[string]string) []byte {
	if m == nil {
		return []byte("{}")
	}
	b, _ := json.Marshal(m)
	return b
}

func (s *Store) CreateConnection(ctx context.Context, c *Connection) error {
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	if c.Groups == nil {
		c.Groups = []string{}
	}
	if c.Kind == "" {
		c.Kind = KindOAuth2
	}
	groups, _ := json.Marshal(c.Groups)
	return s.pool.QueryRow(ctx, `INSERT INTO connections (id, org_id, module, name, account, owner_sub, owner_name,
		scope, groups, access_token, refresh_token, token_type, scopes, expires_at, instance_url, kind, fields, secret_fields)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,''),$16,$17,$18) RETURNING created_at, updated_at`,
		c.ID, c.OrgID, c.Module, c.Name, c.Account, c.OwnerSub, c.OwnerName, c.Scope, groups,
		c.AccessToken, c.RefreshToken, c.TokenType, c.Scopes, c.ExpiresAt, c.InstanceURL,
		c.Kind, fieldsJSON(c.Fields), fieldsJSON(c.SecretFields),
	).Scan(&c.CreatedAt, &c.UpdatedAt)
}

func (s *Store) GetConnection(ctx context.Context, id string) (*Connection, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrNotFound
	}
	return scanConnection(s.pool.QueryRow(ctx, `SELECT `+connCols+` FROM connections WHERE id=$1`, uid))
}

// ListConnections returns the organization's connections; callers filter by access.
func (s *Store) ListConnections(ctx context.Context, org string) ([]*Connection, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+connCols+` FROM connections WHERE org_id=$1 ORDER BY module, name`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateConnectionMeta changes name and sharing.
func (s *Store) UpdateConnectionMeta(ctx context.Context, c *Connection) error {
	groups, _ := json.Marshal(c.Groups)
	tag, err := s.pool.Exec(ctx, `UPDATE connections SET name=$2, scope=$3, groups=$4, updated_at=now() WHERE id=$1`,
		c.ID, c.Name, c.Scope, groups)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateConnectionFields replaces the fields of a credential.
func (s *Store) UpdateConnectionFields(ctx context.Context, c *Connection) error {
	err := s.pool.QueryRow(ctx, `UPDATE connections SET fields=$2, secret_fields=$3, updated_at=now()
		WHERE id=$1 AND kind='credential' RETURNING updated_at`,
		c.ID, fieldsJSON(c.Fields), fieldsJSON(c.SecretFields)).Scan(&c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) DeleteConnection(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM connections WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RefreshConnection locks the connection row and lets fn update its tokens.
// Concurrent refreshes (other workers or replicas) wait for the lock and then
// see the already refreshed tokens, so rotating refresh tokens stay valid.
// fn returns false if nothing needs to be saved.
func (s *Store) RefreshConnection(ctx context.Context, id string, fn func(c *Connection) (bool, error)) (*Connection, error) {
	var out *Connection
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := scanConnection(tx.QueryRow(ctx, `SELECT `+connCols+` FROM connections WHERE id=$1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		changed, err := fn(c)
		if err != nil {
			return err
		}
		if changed {
			if _, err := tx.Exec(ctx, `UPDATE connections SET access_token=$2, refresh_token=$3, token_type=$4,
				expires_at=$5, scopes=$6, instance_url=NULLIF($7,''), updated_at=now() WHERE id=$1`,
				c.ID, c.AccessToken, c.RefreshToken, c.TokenType, c.ExpiresAt, c.Scopes, c.InstanceURL); err != nil {
				return err
			}
		}
		out = c
		return nil
	})
	return out, err
}
