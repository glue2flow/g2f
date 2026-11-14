// Package signing signs and verifies plugin bundles with Ed25519.
//
// A signature covers the uploaded file byte for byte. The signed message is
//
//	"g2f-plugin-v1\n" + "kind:" + kind + "\n" + "sha256:" + hex(sha256(file)) + "\n"
//
// where kind is "zip" or "yaml" (from the file extension), so a signature
// cannot be reused for other bytes or for the same bytes read as another
// format. The signature travels next to the bundle as a small JSON envelope
// (the ".sig" file):
//
//	{"keyId": "acme", "alg": "ed25519", "signature": "<base64>"}
//
// Only the standard library is used, so the format can be reproduced with
// any Ed25519 implementation.
package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

const (
	// Alg is the only supported algorithm.
	Alg = "ed25519"
	// Prefix separates plugin signatures from anything else the key might sign.
	Prefix = "g2f-plugin-v1\n"
	// MaxSignatureSize bounds a signature envelope.
	MaxSignatureSize = 4 << 10
)

var keyIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var (
	// ErrUnknownKey: the signature names a key that is not trusted.
	ErrUnknownKey = errors.New("signed by an unknown key")
	// ErrBadSignature: the signature does not match the bundle.
	ErrBadSignature = errors.New("signature does not match the plugin")
)

// Signature is the detached signature envelope.
type Signature struct {
	KeyID     string `json:"keyId"`
	Alg       string `json:"alg"`
	Signature string `json:"signature"`
}

// Kind returns the bundle kind of a file name: "zip" or "yaml".
func Kind(filename string) (string, error) {
	switch strings.ToLower(path.Ext(strings.ReplaceAll(filename, `\`, "/"))) {
	case ".zip":
		return "zip", nil
	case ".yaml", ".yml":
		return "yaml", nil
	}
	return "", fmt.Errorf("%q is neither a .zip nor a .yaml plugin", filename)
}

// Message is what gets signed for a bundle of the given kind.
func Message(kind string, bundle []byte) []byte {
	sum := sha256.Sum256(bundle)
	return []byte(Prefix + "kind:" + kind + "\nsha256:" + hex.EncodeToString(sum[:]) + "\n")
}

// Sign signs bundle (named filename, for its kind) with key.
func Sign(filename string, bundle []byte, keyID string, key ed25519.PrivateKey) (*Signature, error) {
	if !keyIDRe.MatchString(keyID) {
		return nil, fmt.Errorf("invalid key id %q", keyID)
	}
	kind, err := Kind(filename)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(key, Message(kind, bundle))
	return &Signature{KeyID: keyID, Alg: Alg, Signature: base64.StdEncoding.EncodeToString(sig)}, nil
}

// Marshal encodes the envelope as written to .sig files.
func (s *Signature) Marshal() []byte {
	b, _ := json.MarshalIndent(s, "", "  ")
	return append(b, '\n')
}

// Parse decodes and checks a signature envelope. Unknown fields, other
// algorithms and signatures of the wrong length are refused.
func Parse(data []byte) (*Signature, error) {
	if len(data) > MaxSignatureSize {
		return nil, errors.New("signature file is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Signature
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("invalid signature file: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("invalid signature file: trailing data")
	}
	if err := s.check(); err != nil {
		return nil, fmt.Errorf("invalid signature file: %w", err)
	}
	return &s, nil
}

func (s *Signature) check() error {
	if s.Alg != Alg {
		return fmt.Errorf("alg must be %q", Alg)
	}
	if !keyIDRe.MatchString(s.KeyID) {
		return fmt.Errorf("invalid keyId %q", s.KeyID)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(s.Signature)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return errors.New("signature must be 64 bytes, base64")
	}
	return nil
}

// Keys are trusted public keys by name.
type Keys map[string]ed25519.PublicKey

// Verify checks sig over bundle and returns the signer's key name.
func (k Keys) Verify(filename string, bundle []byte, sig *Signature) (string, error) {
	if err := sig.check(); err != nil {
		return "", err
	}
	pub, ok := k[sig.KeyID]
	if !ok {
		return "", fmt.Errorf("%w %q", ErrUnknownKey, sig.KeyID)
	}
	kind, err := Kind(filename)
	if err != nil {
		return "", err
	}
	raw, _ := base64.StdEncoding.DecodeString(sig.Signature)
	if !ed25519.Verify(pub, Message(kind, bundle), raw) {
		return "", fmt.Errorf("%w (key %q)", ErrBadSignature, sig.KeyID)
	}
	return sig.KeyID, nil
}

// ParseKeys parses "name=base64key,name2=base64key" (G2F_PLUGIN_TRUSTED_KEYS).
func ParseKeys(s string) (Keys, error) {
	keys := Keys{}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, b64, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if !ok || !keyIDRe.MatchString(name) {
			return nil, fmt.Errorf("%q: expected name=base64-public-key with a name of letters, digits, '.', '_' or '-'", entry)
		}
		if _, dup := keys[name]; dup {
			return nil, fmt.Errorf("key name %q is used twice", name)
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(b64))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("key %q: must be a base64 Ed25519 public key (32 bytes)", name)
		}
		keys[name] = ed25519.PublicKey(raw)
	}
	return keys, nil
}

// KeyFile is the private key file written by `g2f plugin keygen`.
type KeyFile struct {
	KeyID string `json:"keyId"`
	Alg   string `json:"alg"`
	// Seed is the 32-byte Ed25519 seed, base64.
	Seed string `json:"privateKey"`
}

// GenerateKey creates a key pair; the public part is returned as the
// "name=base64" entry for G2F_PLUGIN_TRUSTED_KEYS.
func GenerateKey(keyID string) (*KeyFile, string, error) {
	if !keyIDRe.MatchString(keyID) {
		return nil, "", fmt.Errorf("invalid key name %q: letters, digits, '.', '_' or '-'", keyID)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	kf := &KeyFile{KeyID: keyID, Alg: Alg, Seed: base64.StdEncoding.EncodeToString(priv.Seed())}
	return kf, keyID + "=" + base64.StdEncoding.EncodeToString(pub), nil
}

// WriteKeyFile writes a new private key file readable only by its owner. It
// never overwrites an existing file.
func WriteKeyFile(name string, kf *KeyFile) error {
	b, _ := json.MarshalIndent(kf, "", "  ")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadKeyFile loads a private key file.
func ReadKeyFile(name string) (string, ed25519.PrivateKey, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return "", nil, err
	}
	var kf KeyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return "", nil, fmt.Errorf("%s: not a plugin key file: %w", name, err)
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(kf.Seed)
	if kf.Alg != Alg || !keyIDRe.MatchString(kf.KeyID) || err != nil || len(seed) != ed25519.SeedSize {
		return "", nil, fmt.Errorf("%s: not a valid Ed25519 plugin key file", name)
	}
	return kf.KeyID, ed25519.NewKeyFromSeed(seed), nil
}

// PublicEntry returns the "name=base64" trusted-key entry of a private key.
func PublicEntry(keyID string, key ed25519.PrivateKey) string {
	return keyID + "=" + base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
}
