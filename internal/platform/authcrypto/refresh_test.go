package authcrypto

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
)

func TestOpaqueRefreshRoundTrip(t *testing.T) {
	r := NewOpaqueRefresh()
	plain, hash, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) != sha256.Size {
		t.Fatalf("hash len %d", len(hash))
	}
	raw, err := base64.RawURLEncoding.DecodeString(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 32 {
		t.Fatalf("raw len %d", len(raw))
	}
	if stringsContainsPadding(plain) {
		t.Fatalf("plaintext has padding: %q", plain)
	}
	sum := sha256.Sum256(raw)
	if string(hash) != string(sum[:]) {
		t.Fatal("Issue hash must be SHA-256 of the raw 32 bytes")
	}
	got, err := r.Hash(plain)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(hash) {
		t.Fatal("Hash(plaintext) must match Issue hash")
	}
	other, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if other == plain {
		t.Fatal("tokens must be unique")
	}
}

func TestOpaqueRefreshInvalid(t *testing.T) {
	r := NewOpaqueRefresh()
	for _, tok := range []string{
		"",
		"not-base64url!!!",
		base64.RawURLEncoding.EncodeToString([]byte("short")),
		base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"====",
	} {
		if _, err := r.Hash(tok); !errors.Is(err, errInvalidRefresh) {
			t.Fatalf("%q: %v", tok, err)
		}
	}
}

func stringsContainsPadding(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return true
		}
	}
	return false
}
