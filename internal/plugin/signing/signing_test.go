package signing

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T, id string) (Keys, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return Keys{id: pub}, priv
}

type entry struct{ name, data string }

func makeZip(t *testing.T, entries ...entry) []byte {
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

const manifest = "module: demo\nversion: 1.0.0\nbaseUrl: https://example.com\nnodes: [{type: a, name: A, request: {path: /a}}]\n"

func TestSignVerifyRoundTrip(t *testing.T) {
	keys, priv := testKey(t, "acme")
	zipA := makeZip(t, entry{"plugin.yaml", manifest}, entry{"plugin.wasm", "\x00asm"})
	for name, data := range map[string][]byte{"demo.yaml": []byte(manifest), "demo.zip": zipA} {
		sig, err := Sign(name, data, "acme", priv)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := Parse(sig.Marshal())
		if err != nil {
			t.Fatal(err)
		}
		if signer, err := keys.Verify(name, data, parsed); err != nil || signer != "acme" {
			t.Fatalf("%s: %q %v", name, signer, err)
		}
		// Any changed byte fails.
		for _, i := range []int{0, len(data) / 2, len(data) - 1} {
			changed := bytes.Clone(data)
			changed[i] ^= 1
			if _, err := keys.Verify(name, changed, parsed); !errors.Is(err, ErrBadSignature) {
				t.Errorf("%s: byte %d changed: %v", name, i, err)
			}
		}
		// Appending fails too.
		if _, err := keys.Verify(name, append(bytes.Clone(data), '\n'), parsed); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: appended: %v", name, err)
		}
	}

	// The same bytes under the other kind do not verify.
	sig, _ := Sign("demo.yaml", []byte(manifest), "acme", priv)
	if _, err := keys.Verify("demo.zip", []byte(manifest), sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("kind swap: %v", err)
	}
	if _, err := keys.Verify("demo.yml", []byte(manifest), sig); err != nil {
		t.Fatalf(".yml is yaml: %v", err)
	}
}

func TestZipChangesBreakSignature(t *testing.T) {
	keys, priv := testKey(t, "acme")
	a, b := entry{"plugin.yaml", manifest}, entry{"plugin.wasm", "\x00asm-code"}
	orig := makeZip(t, a, b)
	sig, _ := Sign("p.zip", orig, "acme", priv)
	for name, z := range map[string][]byte{
		"added":     makeZip(t, a, b, entry{"extra.txt", "x"}),
		"removed":   makeZip(t, a),
		"reordered": makeZip(t, b, a),
		"duplicate": makeZip(t, a, b, b),
		"renamed":   makeZip(t, entry{"demo/plugin.yaml", manifest}, b),
	} {
		if _, err := keys.Verify("p.zip", z, sig); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A signature for bundle A does not verify bundle B.
	other := makeZip(t, entry{"plugin.yaml", strings.Replace(manifest, "demo", "other", 1)}, b)
	if _, err := keys.Verify("p.zip", other, sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other bundle: %v", err)
	}
}

func TestWrongAndUnknownKeys(t *testing.T) {
	_, priv := testKey(t, "acme")
	otherKeys, _ := testKey(t, "acme") // same name, different key
	sig, _ := Sign("p.yaml", []byte(manifest), "acme", priv)
	if _, err := otherKeys.Verify("p.yaml", []byte(manifest), sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	unknown, _ := testKey(t, "someone")
	if _, err := unknown.Verify("p.yaml", []byte(manifest), sig); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := (Keys{}).Verify("p.yaml", []byte(manifest), sig); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("no keys: %v", err)
	}
	if _, err := Sign("p.txt", []byte(manifest), "acme", priv); err == nil {
		t.Fatal("signed a .txt")
	}
	if _, err := Sign("p.yaml", []byte(manifest), "bad id", priv); err == nil {
		t.Fatal("invalid key id accepted")
	}
}

func TestParseRejectsMalformedSignatures(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(make([]byte, 64))
	for name, in := range map[string]string{
		"empty":       "",
		"not json":    "hello",
		"array":       `[]`,
		"unknown alg": `{"keyId":"a","alg":"rsa","signature":"` + good + `"}`,
		"no alg":      `{"keyId":"a","signature":"` + good + `"}`,
		"no key":      `{"alg":"ed25519","signature":"` + good + `"}`,
		"bad key id":  `{"keyId":"a b","alg":"ed25519","signature":"` + good + `"}`,
		"short":       `{"keyId":"a","alg":"ed25519","signature":"` + base64.StdEncoding.EncodeToString(make([]byte, 63)) + `"}`,
		"not base64":  `{"keyId":"a","alg":"ed25519","signature":"!!"}`,
		"extra field": `{"keyId":"a","alg":"ed25519","signature":"` + good + `","x":1}`,
		"trailing":    `{"keyId":"a","alg":"ed25519","signature":"` + good + `"}{}`,
		"huge":        `{"keyId":"a","alg":"ed25519","signature":"` + good + `"}` + strings.Repeat(" ", MaxSignatureSize),
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte(`{"keyId":"a","alg":"ed25519","signature":"` + good + `"}` + "\n")); err != nil {
		t.Fatal(err)
	}
}

func TestParseKeys(t *testing.T) {
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keys, err := ParseKeys(" acme=" + pub + " , other.org=" + pub + ",")
	if err != nil || len(keys) != 2 {
		t.Fatalf("%v %v", keys, err)
	}
	if keys, err := ParseKeys(""); err != nil || len(keys) != 0 {
		t.Fatalf("empty: %v %v", keys, err)
	}
	for _, in := range []string{
		"acme",             // no key
		"=" + pub,          // no name
		"a b=" + pub,       // invalid name
		"acme=" + pub[:20], // short
		"acme=" + base64.StdEncoding.EncodeToString(make([]byte, 64)), // private key size
		"acme=not-base64!",             // not base64
		"acme=" + pub + ",acme=" + pub, // duplicate
	} {
		if _, err := ParseKeys(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestKeyFiles(t *testing.T) {
	dir := t.TempDir()
	kf, entry, err := GenerateKey("acme")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "acme.key")
	if err := WriteKeyFile(name, kf); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(name); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if err := WriteKeyFile(name, kf); err == nil {
		t.Fatal("overwrote an existing key file")
	}
	id, priv, err := ReadKeyFile(name)
	if err != nil || id != "acme" {
		t.Fatalf("%q %v", id, err)
	}
	if PublicEntry(id, priv) != entry {
		t.Fatal("public entry differs")
	}
	keys, _ := ParseKeys(entry)
	sig, _ := Sign("x.yaml", []byte(manifest), id, priv)
	if _, err := keys.Verify("x.yaml", []byte(manifest), sig); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "bad.key"), []byte(`{"keyId":"a","alg":"ed25519","privateKey":"AAAA"}`), 0o600)
	if _, _, err := ReadKeyFile(filepath.Join(dir, "bad.key")); err == nil {
		t.Fatal("short seed accepted")
	}
}
