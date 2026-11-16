-- API-key credentials live next to OAuth2 accounts, so sharing, the node
-- picker and run-time resolution are shared. kind tells them apart.
ALTER TABLE connections ADD COLUMN kind text NOT NULL DEFAULT 'oauth2' CHECK (kind IN ('oauth2', 'credential'));
-- Non-secret credential fields (name -> value), readable.
ALTER TABLE connections ADD COLUMN fields jsonb NOT NULL DEFAULT '{}';
-- Secret credential fields (name -> value sealed with secrets.CredentialAAD).
ALTER TABLE connections ADD COLUMN secret_fields jsonb NOT NULL DEFAULT '{}';
ALTER TABLE connections ALTER COLUMN access_token SET DEFAULT '';
