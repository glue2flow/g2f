package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/plugin"
	"github.com/glue2flow/g2f/internal/plugin/signing"
)

const testManifest = "module: demo\nname: Demo\nversion: 1.2.0\ndescription: A demo\nbaseUrl: https://example.com\nnodes: [{type: a, name: A, request: {path: /a}}]\n"

func runPlugin(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := pluginCommand(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestPluginCLI(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "acme.key")

	code, entry, stderr := runPlugin(t, "keygen", "--name", "acme", "--out", key)
	if code != 0 || !strings.HasPrefix(entry, "acme=") || !strings.Contains(stderr, "G2F_PLUGIN_TRUSTED_KEYS") {
		t.Fatalf("keygen: %d %q %q", code, entry, stderr)
	}
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi.Mode(), err)
	}
	if code, _, _ := runPlugin(t, "keygen", "--name", "acme", "--out", key); code == 0 {
		t.Fatal("keygen overwrote a key file")
	}
	if code, _, _ := runPlugin(t, "keygen"); code == 0 {
		t.Fatal("keygen without --name")
	}
	keys := strings.TrimSpace(entry)

	bundles := filepath.Join(dir, "bundles")
	_ = os.Mkdir(bundles, 0o755)
	bundle := filepath.Join(bundles, "demo.yaml")
	if err := os.WriteFile(bundle, []byte(testManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, stderr := runPlugin(t, "sign", "--key", key, bundle); code != 0 || !strings.Contains(out, "demo.yaml.sig") {
		t.Fatalf("sign: %d %q %q", code, out, stderr)
	}
	if code, out, stderr := runPlugin(t, "verify", "--keys", keys, bundle); code != 0 || !strings.Contains(out, `signed by "acme"`) {
		t.Fatalf("verify: %d %q %q", code, out, stderr)
	}
	// Flags after the bundle work too.
	if code, _, stderr := runPlugin(t, "verify", bundle, "--keys", keys); code != 0 {
		t.Fatalf("verify, flags last: %q", stderr)
	}

	// Another key, a changed bundle and a missing signature fail.
	_, otherEntry, _ := runPlugin(t, "keygen", "--name", "acme", "--out", filepath.Join(dir, "other.key"))
	if code, _, stderr := runPlugin(t, "verify", "--keys", strings.TrimSpace(otherEntry), bundle); code != 1 || !strings.Contains(stderr, "does not match") {
		t.Fatalf("wrong key: %d %q", code, stderr)
	}
	changed := filepath.Join(dir, "changed.yaml")
	_ = os.WriteFile(changed, []byte(testManifest+"# x\n"), 0o644)
	if code, _, stderr := runPlugin(t, "verify", "--keys", keys, "--sig", bundle+".sig", changed); code != 1 || !strings.Contains(stderr, "does not match") {
		t.Fatalf("changed bundle: %d %q", code, stderr)
	}
	if code, _, _ := runPlugin(t, "verify", "--keys", keys, changed); code != 1 {
		t.Fatal("verified without a signature file")
	}
	if code, _, stderr := runPlugin(t, "verify", "--keys", "acme=short", bundle); code != 1 || !strings.Contains(stderr, "--keys") {
		t.Fatalf("bad keys: %q", stderr)
	}
	// Bundles the server would refuse are not signed.
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("module: x\n"), 0o644)
	if code, _, _ := runPlugin(t, "sign", "--key", key, bad); code == 0 {
		t.Fatal("signed an invalid manifest")
	}

	// Catalog with signatures.
	code, out, stderr := runPlugin(t, "catalog", bundles, "--base-url", "https://plugins.example.com/acme", "--key", key, "--name", "ACME")
	if code != 0 || !strings.Contains(out, "1 plugins") {
		t.Fatalf("catalog: %d %q %q", code, out, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(bundles, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx plugin.CatalogIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Name != "ACME" || len(idx.Plugins) != 1 {
		t.Fatalf("%+v", idx)
	}
	e := idx.Plugins[0]
	if e.Module != "demo" || e.Version != "1.2.0" || e.URL != "https://plugins.example.com/acme/demo.yaml" || e.Signature == nil || len(e.SHA256) != 64 {
		t.Fatalf("%+v", e)
	}
	trusted, _ := signing.ParseKeys(keys)
	data, _ := os.ReadFile(bundle)
	if _, err := trusted.Verify("demo.yaml", data, e.Signature); err != nil {
		t.Fatalf("catalog signature: %v", err)
	}

	// Without a key, the .sig next to the bundle is used; URLs are relative.
	if code, _, stderr := runPlugin(t, "catalog", bundles, "--out", filepath.Join(dir, "rel.json")); code != 0 {
		t.Fatal(stderr)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "rel.json"))
	idx = plugin.CatalogIndex{}
	_ = json.Unmarshal(raw, &idx)
	if e := idx.Plugins[0]; e.URL != "demo.yaml" || e.Signature == nil || e.Signature.KeyID != "acme" {
		t.Fatalf("%+v", e)
	}

	if code, _, _ := runPlugin(t, "nope"); code != 2 {
		t.Fatal("unknown command")
	}
	if code, _, stderr := runPlugin(t, "sign", "--help"); code != 0 || !strings.Contains(stderr, "Usage: g2f plugin sign") {
		t.Fatalf("help: %d %q", code, stderr)
	}
}
