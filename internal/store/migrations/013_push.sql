-- Checks of polling triggers, whoever asks for them (the clock, a push
-- notification, an IMAP IDLE wake-up), run one at a time per trigger across
-- all replicas. A row is the trigger's check queue of length one:
-- requested_at is set by every request and cleared only by a check that
-- started after it, so a request is never lost and bursts coalesce.
CREATE TABLE poll_checks (
    workflow_id  uuid        NOT NULL REFERENCES workflows (id) ON DELETE CASCADE,
    node_id      text        NOT NULL,
    requested_at timestamptz,
    -- The last scheduled slot requested: replicas request each slot once.
    last_slot    timestamptz,
    -- The instance running a check, until lease_until (renewed while it runs).
    owner        uuid,
    lease_until  timestamptz,
    started_at   timestamptz,
    PRIMARY KEY (workflow_id, node_id)
);

CREATE INDEX poll_checks_pending_idx ON poll_checks (requested_at) WHERE requested_at IS NOT NULL;

-- Named leases: one replica at a time does a job (keeps an IMAP IDLE
-- connection of a trigger, reconciles push subscriptions). A lease that is not
-- renewed expires and another replica takes over. state/detail describe what
-- the holder is doing, for the editor.
CREATE TABLE leases (
    name        text        PRIMARY KEY,
    holder      uuid        NOT NULL,
    expires_at  timestamptz NOT NULL,
    acquired_at timestamptz NOT NULL DEFAULT now(),
    state       text        NOT NULL DEFAULT '',
    detail      text        NOT NULL DEFAULT '',
    state_since timestamptz
);

-- Push subscriptions at mail providers (Gmail watches, Microsoft Graph
-- subscriptions), converged with the active triggers by the reconciler.
-- sub_key is "<module>:<connection id>" for subscriptions per account and
-- "<module>:<workflow id>/<node id>" for subscriptions per trigger; triggers
-- lists the triggers a notification wakes.
CREATE TABLE push_subscriptions (
    sub_key              text        PRIMARY KEY,
    org_id               text        NOT NULL,
    module               text        NOT NULL,
    endpoint             text        NOT NULL, -- gmail | graph
    connection_id        uuid        NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    node_type            text        NOT NULL,
    triggers             jsonb       NOT NULL DEFAULT '[]',
    -- Hash of what the subscription was created from (folder, topic, URL):
    -- a change replaces the subscription.
    fingerprint          text        NOT NULL DEFAULT '',
    external_id          text        NOT NULL DEFAULT '',
    -- Sealed shared secret (Graph clientState).
    secret               text        NOT NULL DEFAULT '',
    expires_at           timestamptz,
    renewed_at           timestamptz,
    last_error           text        NOT NULL DEFAULT '',
    failures             integer     NOT NULL DEFAULT 0,
    retry_at             timestamptz,
    last_notification_at timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX push_subscriptions_external_idx ON push_subscriptions (endpoint, external_id) WHERE external_id <> '';
CREATE INDEX push_subscriptions_connection_idx ON push_subscriptions (connection_id);

-- Secret path tokens of public push endpoints, per organization. The hash
-- finds the organization; the sealed token lets admins see the URL again.
CREATE TABLE push_tokens (
    org_id       text        NOT NULL,
    endpoint     text        NOT NULL,
    token_hash   bytea       NOT NULL UNIQUE,
    token_sealed text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, endpoint)
);
