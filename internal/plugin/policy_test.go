package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/plugin/signing"
	"github.com/glue2flow/g2f/internal/store"
)

type storeRecord = store.PluginRecord

func digestOf(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

type zipEntry struct{ name, data string }

// orderedZip writes entries in order, duplicates included.
func orderedZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(e.data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func yamlPlugin(module, version string) []byte {
	return []byte(fmt.Sprintf("module: %s\nname: %s\nversion: %s\nbaseUrl: https://example.com\nnodes: [{type: get, name: Get, request: {path: /x}}]\n",
		module, strings.ToUpper(module[:1])+module[1:], version))
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func sign(t *testing.T, filename string, data []byte, keyID string, key ed25519.PrivateKey) []byte {
	t.Helper()
	s, err := signing.Sign(filename, data, keyID, key)
	if err != nil {
		t.Fatal(err)
	}
	return s.Marshal()
}

func TestImportSignaturePolicy(t *testing.T) {
	acmePub, acme := newKey(t)
	_, stranger := newKey(t)
	_, impostor := newKey(t) // signs as "acme" with the wrong key
	data := yamlPlugin("demo", "1.0.0")
	sigs := map[string][]byte{
		"unsigned":    nil,
		"valid":       sign(t, "demo.yaml", data, "acme", acme),
		"invalid":     sign(t, "demo.yaml", data, "acme", impostor),
		"unknown key": sign(t, "demo.yaml", data, "stranger", stranger),
		"garbage":     []byte("not a signature"),
		"other file":  sign(t, "demo.yaml", yamlPlugin("demo", "1.0.1"), "acme", acme),
	}
	// want: "" = imported (with the signer), otherwise part of the error.
	matrix := map[bool]map[string]string{
		false: {"unsigned": "", "valid": "", "invalid": "does not match", "unknown key": "unknown key", "garbage": "invalid signature", "other file": "does not match"},
		true:  {"unsigned": "not signed", "valid": "", "invalid": "does not match", "unknown key": "unknown key", "garbage": "invalid signature", "other file": "does not match"},
	}
	for _, require := range []bool{false, true} {
		for name, sig := range sigs {
			t.Run(fmt.Sprintf("require=%v/%s", require, name), func(t *testing.T) {
				reg := nodes.NewRegistry()
				st := newMemStore()
				mgr := NewManager(reg, st, discard)
				mgr.SetTrust(Trust{Keys: signing.Keys{"acme": acmePub}, RequireSigned: require})
				info, err := mgr.Import(context.Background(), "demo.yaml", data, "alice", ImportOptions{Signature: sig})
				want := matrix[require][name]
				if want == "" {
					if err != nil {
						t.Fatal(err)
					}
					wantSigner := map[string]string{"valid": "acme"}[name]
					if info.Signer != wantSigner || st.plugins["demo"].Signer != wantSigner {
						t.Fatalf("signer %q, stored %q, want %q", info.Signer, st.plugins["demo"].Signer, wantSigner)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("got %v, want error containing %q", err, want)
				}
				if reg.Exists("demo.get") || len(st.plugins) != 0 {
					t.Fatal("refused plugin was installed or stored")
				}
			})
		}
	}
}

func TestImportReplaceNeedsConfirmation(t *testing.T) {
	acmePub, acme := newKey(t)
	reg := nodes.NewRegistry()
	st := newMemStore()
	mgr := NewManager(reg, st, discard)
	mgr.SetTrust(Trust{Keys: signing.Keys{"acme": acmePub}})
	ctx := context.Background()
	v1 := yamlPlugin("github", "1.0.0")
	if _, err := mgr.Import(ctx, "github.yaml", v1, "alice", ImportOptions{Signature: sign(t, "github.yaml", v1, "acme", acme)}); err != nil {
		t.Fatal(err)
	}
	v2 := yamlPlugin("github", "1.1.0")
	_, err := mgr.Import(ctx, "github.yaml", v2, "mallory", ImportOptions{})
	var exists *ExistsError
	if !errors.As(err, &exists) || !errors.Is(err, ErrExists) {
		t.Fatalf("want ExistsError, got %v", err)
	}
	if exists.Current.Version != "1.0.0" || exists.Current.Signer != "acme" || exists.Incoming.Version != "1.1.0" || exists.Incoming.Signer != "" {
		t.Fatalf("conflict %+v", exists)
	}
	if info, _ := reg.Module("github"); info.Version != "1.0.0" || st.plugins["github"].Version != "1.0.0" {
		t.Fatal("plugin replaced without confirmation")
	}
	info, err := mgr.Import(ctx, "github.yaml", v2, "alice", ImportOptions{Replace: true})
	if err != nil || info.Version != "1.1.0" || info.Signer != "" || st.plugins["github"].Version != "1.1.0" {
		t.Fatalf("replace: %+v %v", info, err)
	}

	// Another replica imported the module first: the database refuses.
	mgr2 := NewManager(nodes.NewRegistry(), st, discard)
	if _, err := mgr2.Import(ctx, "github.yaml", v1, "bob", ImportOptions{}); !errors.Is(err, ErrExists) {
		t.Fatalf("cross-replica import: %v", err)
	}

	// A catalog install must contain the module it was listed as.
	if _, err := mgr.Import(ctx, "x.yaml", yamlPlugin("slack", "1.0.0"), "alice", ImportOptions{Module: "github", Replace: true}); err == nil {
		t.Fatal("module mismatch accepted")
	}
}

func TestImportDatabaseFailureChangesNothing(t *testing.T) {
	reg := nodes.NewRegistry()
	st := newMemStore()
	mgr := NewManager(reg, st, discard)
	ctx := context.Background()
	if _, err := mgr.Import(ctx, "demo.yaml", yamlPlugin("demo", "1.0.0"), "alice", ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	st.saveErr = errors.New("database down")
	if _, err := mgr.Import(ctx, "demo.yaml", yamlPlugin("demo", "2.0.0"), "alice", ImportOptions{Replace: true}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := mgr.Import(ctx, "new.yaml", yamlPlugin("fresh", "1.0.0"), "alice", ImportOptions{}); err == nil {
		t.Fatal("expected error")
	}
	if info, _ := reg.Module("demo"); info.Version != "1.0.0" {
		t.Fatalf("registry changed: %s", info.Version)
	}
	if _, ok := reg.Module("fresh"); ok {
		t.Fatal("new plugin registered although it was not saved")
	}
}

func TestSyncReverifiesStoredPlugins(t *testing.T) {
	acmePub, acme := newKey(t)
	trust := Trust{Keys: signing.Keys{"acme": acmePub}}
	st := newMemStore()
	ctx := context.Background()
	mgr := NewManager(nodes.NewRegistry(), st, discard)
	mgr.SetTrust(trust)
	signed := yamlPlugin("signed", "1.0.0")
	if _, err := mgr.Import(ctx, "signed.yaml", signed, "alice", ImportOptions{Signature: sign(t, "signed.yaml", signed, "acme", acme)}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Import(ctx, "plain.yaml", yamlPlugin("plain", "1.0.0"), "alice", ImportOptions{}); err != nil {
		t.Fatal(err)
	}

	// Another replica with the same policy loads both and knows the signer.
	reg2 := nodes.NewRegistry()
	mgr2 := NewManager(reg2, st, discard)
	mgr2.SetTrust(trust)
	if err := mgr2.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if info, ok := reg2.Module("signed"); !ok || info.Signer != "acme" {
		t.Fatalf("signed: %+v", info)
	}
	if !reg2.Exists("plain.get") {
		t.Fatal("unsigned plugin not loaded without require-signed")
	}

	// The operator turns require-signed on: after the restart the unsigned
	// plugin is blocked and listed with the reason, the signed one loads.
	reg3 := nodes.NewRegistry()
	mgr3 := NewManager(reg3, st, discard)
	mgr3.SetTrust(Trust{Keys: trust.Keys, RequireSigned: true})
	if err := mgr3.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if reg3.Exists("plain.get") || !reg3.Exists("signed.get") {
		t.Fatal("wrong plugins loaded with require-signed")
	}
	blocked := map[string]string{}
	for _, m := range mgr3.Modules() {
		blocked[m.Module] = m.Blocked
	}
	if !strings.Contains(blocked["plain"], "not signed") || blocked["signed"] != "" {
		t.Fatalf("blocked states %v", blocked)
	}
	if cur, ok := mgr3.Installed("plain"); !ok || cur.Blocked == "" {
		t.Fatalf("installed state %+v", cur)
	}
	// Re-importing a blocked plugin needs replace, and it must be signed.
	if _, err := mgr3.Import(ctx, "plain.yaml", yamlPlugin("plain", "1.0.0"), "alice", ImportOptions{}); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("unsigned re-import: %v", err)
	}
	plain := yamlPlugin("plain", "1.0.1")
	if _, err := mgr3.Import(ctx, "plain.yaml", plain, "alice", ImportOptions{Signature: sign(t, "plain.yaml", plain, "acme", acme)}); !errors.Is(err, ErrExists) {
		t.Fatalf("re-import without replace: %v", err)
	}
	if _, err := mgr3.Import(ctx, "plain.yaml", plain, "alice", ImportOptions{Replace: true, Signature: sign(t, "plain.yaml", plain, "acme", acme)}); err != nil {
		t.Fatal(err)
	}
	if !reg3.Exists("plain.get") {
		t.Fatal("signed re-import not loaded")
	}
	for _, m := range mgr3.Modules() {
		if m.Blocked != "" {
			t.Fatalf("%s still blocked", m.Module)
		}
	}
	// Deleting works for blocked plugins too (here: none left).
	if err := mgr3.Delete(ctx, "plain"); err != nil {
		t.Fatal(err)
	}
}

func TestSyncBlocksTamperedRows(t *testing.T) {
	acmePub, acme := newKey(t)
	trust := Trust{Keys: signing.Keys{"acme": acmePub}, RequireSigned: true}
	st := newMemStore()
	ctx := context.Background()
	mgr := NewManager(nodes.NewRegistry(), st, discard)
	mgr.SetTrust(trust)
	data := yamlPlugin("demo", "1.0.0")
	if _, err := mgr.Import(ctx, "demo.yaml", data, "alice", ImportOptions{Signature: sign(t, "demo.yaml", data, "acme", acme)}); err != nil {
		t.Fatal(err)
	}
	orig := *st.plugins["demo"]

	evil := []byte(strings.Replace(string(data), "https://example.com", "https://evil.example", 1))
	for name, tamper := range map[string]func(r *storeRecord){
		// The bundle is swapped and the digest fixed up: the signature fails.
		"bundle": func(r *storeRecord) { r.Bundle = evil; r.SHA256 = digestOf(evil) },
		// The signature is removed: unsigned is refused.
		"signature": func(r *storeRecord) { r.Signature, r.Signer = "", "" },
		// Only the bundle changes: the digest no longer matches.
		"digest": func(r *storeRecord) { r.Bundle = evil },
		// Bundle without a trusted key's signature.
		"resigned": func(r *storeRecord) {
			_, other := newKey(t)
			r.Bundle, r.SHA256, r.Signature = evil, digestOf(evil), string(sign(t, "demo.yaml", evil, "acme", other))
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := orig
			tamper(&rec)
			st.plugins["demo"] = &rec
			reg := nodes.NewRegistry()
			m := NewManager(reg, st, discard)
			m.SetTrust(trust)
			if err := m.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if reg.Exists("demo.get") {
				t.Fatal("tampered plugin loaded")
			}
			if mods := m.Modules(); len(mods) != 1 || mods[0].Blocked == "" {
				t.Fatalf("not reported as blocked: %+v", mods)
			}
		})
	}

	// A replica that had the good version unloads it when the row changes.
	reg := nodes.NewRegistry()
	m := NewManager(reg, st, discard)
	m.SetTrust(trust)
	st.plugins["demo"] = &orig
	if err := m.Sync(ctx); err != nil || !reg.Exists("demo.get") {
		t.Fatalf("good row: %v", err)
	}
	bad := orig
	bad.Bundle, bad.SHA256 = evil, digestOf(evil)
	st.plugins["demo"] = &bad
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if reg.Exists("demo.get") {
		t.Fatal("old version stayed loaded after the row was tampered with")
	}
}

func TestLegacyRowWithoutBundleIsBlocked(t *testing.T) {
	st := newMemStore()
	st.plugins["old"] = &storeRecord{Module: "old", Version: "0.1.0", Manifest: yamlPlugin("old", "0.1.0"), SHA256: "x"}
	reg := nodes.NewRegistry()
	m := NewManager(reg, st, discard)
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	mods := m.Modules()
	if reg.Exists("old.get") || len(mods) != 1 || !strings.Contains(mods[0].Blocked, "import it again") || mods[0].Name != "Old" {
		t.Fatalf("%+v", mods)
	}
}

func TestBundleLimits(t *testing.T) {
	base := "module: lim\nruntime: wasm\nnodes: [{type: a, name: A}]\n"
	for name, extra := range map[string]string{
		"memory too large": "memoryMB: 4096\n",
		"memory negative":  "memoryMB: -1\n",
		"timeout too long": "timeout: 1h\n",
		"timeout zero":     "timeout: 0s\n",
	} {
		z := iconZip(t, map[string]string{"plugin.yaml": base + extra, "plugin.wasm": "\x00asm"})
		if _, err := ParseBundle("lim.zip", z); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	z := iconZip(t, map[string]string{"plugin.yaml": base + "memoryMB: 1024\ntimeout: 5m\n", "plugin.wasm": "\x00asm"})
	if _, err := ParseBundle("lim.zip", z); err != nil {
		t.Fatalf("limits themselves refused: %v", err)
	}

	files := map[string]string{"plugin.yaml": base, "plugin.wasm": "\x00asm"}
	for i := 0; i < maxZipEntries; i++ {
		files[fmt.Sprintf("pad/%d.txt", i)] = "x"
	}
	if _, err := ParseBundle("lim.zip", iconZip(t, files)); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("entry count: %v", err)
	}

	for name, entries := range map[string][]zipEntry{
		"duplicate path":     {{"plugin.yaml", base}, {"plugin.wasm", "a"}, {"plugin.wasm", "b"}},
		"two manifests":      {{"a/plugin.yaml", base}, {"b/plugin.yaml", base}, {"plugin.wasm", "a"}},
		"plugin+manifest":    {{"plugin.yaml", base}, {"manifest.yaml", base}, {"plugin.wasm", "a"}},
		"two wasm files":     {{"plugin.yaml", base}, {"a/plugin.wasm", "a"}, {"b/plugin.wasm", "b"}},
		"dot-slash variants": {{"plugin.yaml", base}, {"plugin.wasm", "a"}, {"./plugin.wasm", "b"}},
	} {
		if _, err := ParseBundle("x.zip", orderedZip(t, entries)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
