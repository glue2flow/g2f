-- Icon files of imported plugins (path -> data URI), referenced by the manifest.
ALTER TABLE plugins ADD COLUMN icons jsonb NOT NULL DEFAULT '{}';
