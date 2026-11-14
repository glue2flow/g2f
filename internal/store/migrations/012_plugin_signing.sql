-- Imported plugins keep the uploaded bundle byte for byte, so every replica
-- can check its signature before loading it. manifest stays for listing
-- plugins that are blocked.
ALTER TABLE plugins ADD COLUMN bundle bytea;
ALTER TABLE plugins ADD COLUMN filename text NOT NULL DEFAULT '';
-- Detached signature (JSON envelope) and the trusted key name it verified with.
ALTER TABLE plugins ADD COLUMN signature text NOT NULL DEFAULT '';
ALTER TABLE plugins ADD COLUMN signer text NOT NULL DEFAULT '';
-- Imported YAML manifests were stored verbatim; WASM bundles cannot be
-- reassembled and stay blocked until imported again.
UPDATE plugins SET bundle = convert_to(manifest, 'UTF8'), filename = module || '.yaml',
    sha256 = encode(sha256(convert_to(manifest, 'UTF8')), 'hex')
    WHERE wasm IS NULL;
ALTER TABLE plugins DROP COLUMN wasm;
ALTER TABLE plugins DROP COLUMN icons;
