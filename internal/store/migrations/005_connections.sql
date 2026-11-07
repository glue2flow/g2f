-- Connected OAuth2 accounts. Tokens are sealed with secrets.Box.
CREATE TABLE connections (
    id            uuid PRIMARY KEY,
    org_id        text        NOT NULL,
    module        text        NOT NULL,
    name          text        NOT NULL,
    account       text        NOT NULL DEFAULT '',
    owner_sub     text        NOT NULL,
    owner_name    text        NOT NULL DEFAULT '',
    scope         text        NOT NULL DEFAULT 'private' CHECK (scope IN ('private', 'group', 'org')),
    groups        jsonb       NOT NULL DEFAULT '[]',
    access_token  text        NOT NULL,
    refresh_token text        NOT NULL DEFAULT '',
    token_type    text        NOT NULL DEFAULT 'Bearer',
    scopes        text        NOT NULL DEFAULT '',
    expires_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX connections_org_module_idx ON connections (org_id, module);
