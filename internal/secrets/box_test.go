package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func key() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func TestSealOpen(t *testing.T) {
	b, err := NewBox(key(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Seal("t0ken", NodeAAD("wf", "n", "p"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.Open(sealed, NodeAAD("wf", "n", "p")); err != nil || got != "t0ken" {
		t.Fatalf("open: %q %v", got, err)
	}
	// Ciphertext moved to another row must not decrypt.
	if _, err := b.Open(sealed, NodeAAD("wf", "other", "p")); err == nil {
		t.Fatal("expected AAD mismatch")
	}
}

func TestRotation(t *testing.T) {
	oldKey, newKey := key(), key()
	oldBox, _ := NewBox(oldKey, nil)
	sealed, _ := oldBox.Seal("v", "aad")

	rotated, err := NewBox(newKey, []string{oldKey})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Open(sealed, "aad"); err != nil || got != "v" {
		t.Fatalf("old value not readable after rotation: %v", err)
	}
	resealed, _ := rotated.Seal("v", "aad")
	if _, err := oldBox.Open(resealed, "aad"); err == nil {
		t.Fatal("new values must use the new key")
	}
	if _, err := NewBox("short", nil); err == nil {
		t.Fatal("expected invalid key error")
	}
}
