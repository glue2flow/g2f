// Package secrets encrypts sensitive values (node secrets, plugin config) at rest.
//
// Values are sealed with AES-256-GCM. Every ciphertext is bound to its storage
// location through additional authenticated data (e.g. "node/<wf>/<node>/<param>"),
// so a ciphertext copied into another row fails to decrypt.
//
// Format: "<keyID>:<base64(nonce || ciphertext)>". keyID identifies which key
// sealed the value, which allows key rotation: configure the new key as
// G2F_SECRET_KEY and keep previous keys in G2F_SECRET_KEYS_OLD.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var ErrUndecryptable = errors.New("secret cannot be decrypted with the configured keys")

// Box seals and opens secret values.
type Box struct {
	current string // key id
	keys    map[string]cipher.AEAD
}

// NewBox builds a Box from a base64 encoded 32-byte key and optional old keys.
func NewBox(current string, old []string) (*Box, error) {
	b := &Box{keys: map[string]cipher.AEAD{}}
	id, err := b.add(current)
	if err != nil {
		return nil, fmt.Errorf("G2F_SECRET_KEY: %w", err)
	}
	b.current = id
	for _, k := range old {
		if _, err := b.add(k); err != nil {
			return nil, fmt.Errorf("G2F_SECRET_KEYS_OLD: %w", err)
		}
	}
	return b, nil
}

func (b *Box) add(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", errors.New("must be base64 (generate with: openssl rand -base64 32)")
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("must decode to 32 bytes, got %d", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:4])
	b.keys[id] = aead
	return id, nil
}

// Seal encrypts plaintext bound to aad.
func (b *Box) Seal(plaintext, aad string) (string, error) {
	aead := b.keys[b.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return b.current + ":" + base64.StdEncoding.EncodeToString(ct), nil
}

// Open decrypts a value produced by Seal with the same aad.
func (b *Box) Open(sealed, aad string) (string, error) {
	id, payload, ok := strings.Cut(sealed, ":")
	aead := b.keys[id]
	if !ok || aead == nil {
		return "", ErrUndecryptable
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(raw) < aead.NonceSize() {
		return "", ErrUndecryptable
	}
	pt, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(aad))
	if err != nil {
		return "", ErrUndecryptable
	}
	return string(pt), nil
}

// NodeAAD is the binding for a per-node secret parameter.
func NodeAAD(workflowID, nodeID, param string) string {
	return "node/" + workflowID + "/" + nodeID + "/" + param
}

// ConfigAAD is the binding for a plugin config value of an organization.
func ConfigAAD(org, module, key string) string {
	return "config/" + org + "/" + module + "/" + key
}
