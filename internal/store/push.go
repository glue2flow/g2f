package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TriggerRef names one trigger node of a workflow.
type TriggerRef struct {
	WorkflowID string `json:"workflowId"`
	NodeID     string `json:"nodeId"`
}

// --- serialized checks of polling triggers ----------------------------------

// RequestCheck asks for a check of a polling trigger. Requests made while a
// check runs make exactly one more check afterwards.
func (s *Store) RequestCheck(ctx context.Context, workflowID, nodeID string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO poll_checks (workflow_id, node_id, requested_at) VALUES ($1, $2, now())
		ON CONFLICT (workflow_id, node_id) DO UPDATE SET requested_at = now()`, workflowID, nodeID)
	return err
}

// RequestSlot asks for the scheduled check of slot. Every replica computes the
// same slots; only the first request of a slot counts (true).
func (s *Store) RequestSlot(ctx context.Context, workflowID, nodeID string, slot time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO poll_checks (workflow_id, node_id, requested_at, last_slot) VALUES ($1, $2, now(), $3)
		ON CONFLICT (workflow_id, node_id) DO UPDATE SET requested_at = now(), last_slot = $3
		WHERE poll_checks.last_slot IS NULL OR poll_checks.last_slot < $3`, workflowID, nodeID, slot)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// AcquireCheck lets owner run the requested check of a trigger. It fails when
// nothing is requested (a check that started after the request covered it)
// or another instance is running a check.
func (s *Store) AcquireCheck(ctx context.Context, workflowID, nodeID, owner string, ttl time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE poll_checks SET owner=$3, lease_until=now()+make_interval(secs => $4), started_at=now()
		WHERE workflow_id=$1 AND node_id=$2 AND requested_at IS NOT NULL AND (owner IS NULL OR lease_until < now())`,
		workflowID, nodeID, owner, ttl.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseCheck ends owner's check. done means the check ran: requests made
// before it started are then answered. It reports whether a request arrived
// meanwhile (or, without done, is still open), so another check is due.
func (s *Store) ReleaseCheck(ctx context.Context, workflowID, nodeID, owner string, done bool) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `UPDATE poll_checks SET owner=NULL, lease_until=NULL,
		requested_at = CASE WHEN $4 AND requested_at <= started_at THEN NULL ELSE requested_at END
		WHERE workflow_id=$1 AND node_id=$2 AND owner=$3 RETURNING requested_at IS NOT NULL`,
		workflowID, nodeID, owner, done).Scan(&pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // the lease expired and someone else took over
	}
	return pending, err
}

// RenewChecks extends the leases of owner's running checks.
func (s *Store) RenewChecks(ctx context.Context, owner string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx, `UPDATE poll_checks SET lease_until=now()+make_interval(secs => $2) WHERE owner=$1`, owner, ttl.Seconds())
	return err
}

// PendingChecks returns requested checks that nobody runs, oldest first:
// requests whose replica was busy, stopped or failed to start the check.
func (s *Store) PendingChecks(ctx context.Context, limit int) ([]TriggerRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT workflow_id::text, node_id FROM poll_checks
		WHERE requested_at IS NOT NULL AND (owner IS NULL OR lease_until < now())
		ORDER BY requested_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TriggerRef, error) {
		var t TriggerRef
		err := r.Scan(&t.WorkflowID, &t.NodeID)
		return t, err
	})
}

// --- leases -----------------------------------------------------------------

// Lease is a named job held by one instance until it expires.
type Lease struct {
	Name       string     `json:"name"`
	Holder     string     `json:"holder"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	State      string     `json:"state"`
	Detail     string     `json:"detail"`
	StateSince *time.Time `json:"stateSince,omitempty"`
}

// AcquireLease takes the lease name for holder if it is free, expired or
// already held by holder.
func (s *Store) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO leases (name, holder, expires_at) VALUES ($1, $2, now()+make_interval(secs => $3))
		ON CONFLICT (name) DO UPDATE SET holder=$2, expires_at=now()+make_interval(secs => $3),
			acquired_at = CASE WHEN leases.holder=$2 THEN leases.acquired_at ELSE now() END,
			state = CASE WHEN leases.holder=$2 THEN leases.state ELSE '' END,
			detail = CASE WHEN leases.holder=$2 THEN leases.detail ELSE '' END,
			state_since = CASE WHEN leases.holder=$2 THEN leases.state_since ELSE NULL END
		WHERE leases.holder=$2 OR leases.expires_at < now()`, name, holder, ttl.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RenewLeases extends holder's leases among names and returns those it still holds.
func (s *Store) RenewLeases(ctx context.Context, holder string, names []string, ttl time.Duration) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `UPDATE leases SET expires_at=now()+make_interval(secs => $3)
		WHERE holder=$1 AND name = ANY($2) RETURNING name`, holder, names, ttl.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ReleaseLease gives up holder's lease.
func (s *Store) ReleaseLease(ctx context.Context, name, holder string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM leases WHERE name=$1 AND holder=$2`, name, holder)
	return err
}

// SetLeaseState records what the holder of a lease is doing. state_since
// changes only with the state.
func (s *Store) SetLeaseState(ctx context.Context, name, holder, state, detail string) error {
	_, err := s.pool.Exec(ctx, `UPDATE leases SET detail=$4,
		state_since = CASE WHEN state=$3 THEN state_since ELSE now() END, state=$3
		WHERE name=$1 AND holder=$2`, name, holder, state, detail)
	return err
}

// GetLease returns a lease that has not expired.
func (s *Store) GetLease(ctx context.Context, name string) (*Lease, error) {
	var l Lease
	err := s.pool.QueryRow(ctx, `SELECT name, holder::text, expires_at, state, detail, state_since FROM leases
		WHERE name=$1 AND expires_at > now()`, name).Scan(&l.Name, &l.Holder, &l.ExpiresAt, &l.State, &l.Detail, &l.StateSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &l, err
}

// --- push subscriptions -----------------------------------------------------

// PushSubscription is a Gmail watch or Microsoft Graph subscription. Secret
// is sealed and never serialized.
type PushSubscription struct {
	Key                string       `json:"key"`
	OrgID              string       `json:"orgId"`
	Module             string       `json:"module"`
	Endpoint           string       `json:"endpoint"`
	ConnectionID       string       `json:"connectionId"`
	NodeType           string       `json:"nodeType"`
	Triggers           []TriggerRef `json:"triggers"`
	Fingerprint        string       `json:"fingerprint"`
	ExternalID         string       `json:"externalId"`
	Secret             string       `json:"-"`
	ExpiresAt          *time.Time   `json:"expiresAt,omitempty"`
	RenewedAt          *time.Time   `json:"renewedAt,omitempty"`
	LastError          string       `json:"lastError,omitempty"`
	Failures           int          `json:"failures"`
	RetryAt            *time.Time   `json:"retryAt,omitempty"`
	LastNotificationAt *time.Time   `json:"lastNotificationAt,omitempty"`
	CreatedAt          time.Time    `json:"createdAt"`
}

const pushCols = `sub_key, org_id, module, endpoint, connection_id::text, node_type, triggers, fingerprint,
	external_id, secret, expires_at, renewed_at, last_error, failures, retry_at, last_notification_at, created_at`

func scanPush(row pgx.Row) (*PushSubscription, error) {
	var p PushSubscription
	var triggers []byte
	err := row.Scan(&p.Key, &p.OrgID, &p.Module, &p.Endpoint, &p.ConnectionID, &p.NodeType, &triggers, &p.Fingerprint,
		&p.ExternalID, &p.Secret, &p.ExpiresAt, &p.RenewedAt, &p.LastError, &p.Failures, &p.RetryAt, &p.LastNotificationAt, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(triggers, &p.Triggers); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) queryPush(ctx context.Context, where string, args ...any) ([]*PushSubscription, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pushCols+` FROM push_subscriptions `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PushSubscription
	for rows.Next() {
		p, err := scanPush(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPushSubscriptions returns all subscriptions (reconciler).
func (s *Store) ListPushSubscriptions(ctx context.Context) ([]*PushSubscription, error) {
	return s.queryPush(ctx, `ORDER BY sub_key`)
}

// GetPushSubscription returns one subscription by key.
func (s *Store) GetPushSubscription(ctx context.Context, key string) (*PushSubscription, error) {
	list, err := s.queryPush(ctx, `WHERE sub_key=$1`, key)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return list[0], nil
}

// PushSubscriptionByExternalID finds the subscription a notification is for.
func (s *Store) PushSubscriptionByExternalID(ctx context.Context, endpoint, id string) (*PushSubscription, error) {
	list, err := s.queryPush(ctx, `WHERE endpoint=$1 AND external_id=$2 AND external_id <> ''`, endpoint, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return list[0], nil
}

// PushSubscriptionsForAccount returns an organization's subscriptions of
// endpoint whose connection's account name is account (case-insensitive).
func (s *Store) PushSubscriptionsForAccount(ctx context.Context, org, endpoint, account string) ([]*PushSubscription, error) {
	return s.queryPush(ctx, `WHERE org_id=$1 AND endpoint=$2 AND connection_id IN (
		SELECT id FROM connections WHERE org_id=$1 AND kind='oauth2' AND lower(account)=lower($3) AND account <> '')`,
		org, endpoint, account)
}

// SavePushSubscription inserts or replaces a subscription.
func (s *Store) SavePushSubscription(ctx context.Context, p *PushSubscription) error {
	if p.Triggers == nil {
		p.Triggers = []TriggerRef{}
	}
	triggers, _ := json.Marshal(p.Triggers)
	return s.pool.QueryRow(ctx, `INSERT INTO push_subscriptions (sub_key, org_id, module, endpoint, connection_id, node_type,
			triggers, fingerprint, external_id, secret, expires_at, renewed_at, last_error, failures, retry_at, last_notification_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (sub_key) DO UPDATE SET org_id=$2, module=$3, endpoint=$4, connection_id=$5, node_type=$6, triggers=$7,
			fingerprint=$8, external_id=$9, secret=$10, expires_at=$11, renewed_at=$12, last_error=$13, failures=$14, retry_at=$15
		RETURNING created_at`,
		p.Key, p.OrgID, p.Module, p.Endpoint, p.ConnectionID, p.NodeType, triggers, p.Fingerprint, p.ExternalID, p.Secret,
		p.ExpiresAt, p.RenewedAt, p.LastError, p.Failures, p.RetryAt, p.LastNotificationAt).Scan(&p.CreatedAt)
}

// DeletePushSubscription removes a subscription.
func (s *Store) DeletePushSubscription(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE sub_key=$1`, key)
	return err
}

// PushNotified records that a notification for the subscription arrived.
func (s *Store) PushNotified(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `UPDATE push_subscriptions SET last_notification_at=now() WHERE sub_key=$1`, key)
	return err
}

// PushRenewNow makes the reconciler renew (recreate: also forget the
// provider's id) a subscription at its next pass.
func (s *Store) PushRenewNow(ctx context.Context, key string, recreate bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE push_subscriptions SET renewed_at=NULL, retry_at=NULL,
		external_id = CASE WHEN $2 THEN '' ELSE external_id END WHERE sub_key=$1`, key, recreate)
	return err
}

// HealthyPushTriggers lists the triggers that currently receive push: an IMAP
// IDLE connection (lease "listen:<workflow>/<node>" in state connected) or a
// live provider subscription without error.
func (s *Store) HealthyPushTriggers(ctx context.Context) ([]TriggerRef, error) {
	var out []TriggerRef
	rows, err := s.pool.Query(ctx, `SELECT name FROM leases WHERE name LIKE 'listen:%' AND state='connected' AND expires_at > now()`)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if wf, node, ok := strings.Cut(strings.TrimPrefix(n, "listen:"), "/"); ok {
			out = append(out, TriggerRef{WorkflowID: wf, NodeID: node})
		}
	}
	subs, err := s.queryPush(ctx, `WHERE external_id <> '' AND last_error = '' AND expires_at > now()`)
	if err != nil {
		return nil, err
	}
	for _, p := range subs {
		out = append(out, p.Triggers...)
	}
	return out, nil
}

// --- push endpoint tokens ---------------------------------------------------

// PushToken returns the sealed endpoint token of an organization.
func (s *Store) PushToken(ctx context.Context, org, endpoint string) (string, error) {
	var sealed string
	err := s.pool.QueryRow(ctx, `SELECT token_sealed FROM push_tokens WHERE org_id=$1 AND endpoint=$2`, org, endpoint).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return sealed, err
}

// CreatePushToken stores a token unless the organization has one already;
// it returns the stored (sealed) token either way.
func (s *Store) CreatePushToken(ctx context.Context, org, endpoint string, hash []byte, sealed string) (string, error) {
	if _, err := s.pool.Exec(ctx, `INSERT INTO push_tokens (org_id, endpoint, token_hash, token_sealed) VALUES ($1,$2,$3,$4)
		ON CONFLICT (org_id, endpoint) DO NOTHING`, org, endpoint, hash, sealed); err != nil {
		return "", err
	}
	return s.PushToken(ctx, org, endpoint)
}

// OrgForPushToken returns the organization whose endpoint token has hash.
func (s *Store) OrgForPushToken(ctx context.Context, endpoint string, hash []byte) (string, error) {
	var org string
	err := s.pool.QueryRow(ctx, `SELECT org_id FROM push_tokens WHERE endpoint=$1 AND token_hash=$2`, endpoint, hash).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return org, err
}
