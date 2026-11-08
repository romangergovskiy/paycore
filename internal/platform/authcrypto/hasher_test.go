package authcrypto

import (
	"encoding/base64"
	"strings"
	"testing"
)

const (
	validPHCSalt = "cGF5Y29yZS1kdW1teS0xNg"
	validPHCKey  = "jqoWTfRQ64xQMhdCUzMQAlFgF9MuueyMrkUvZ9p21gw"
)

func TestArgon2idHashVerify(t *testing.T) {
	h := NewArgon2idHasher()
	encoded, err := h.Hash("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("PHC = %q", encoded)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		t.Fatalf("parts = %d", len(parts))
	}
	ok, err := h.Verify("correct-horse-battery", encoded)
	if err != nil || !ok {
		t.Fatalf("verify got %v %v", ok, err)
	}
	ok, err = h.Verify("wrong-password", encoded)
	if err != nil || ok {
		t.Fatalf("wrong password got %v %v", ok, err)
	}
	other, err := h.Hash("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if other == encoded {
		t.Fatal("salt must be unique per hash")
	}
}

func TestArgon2idInvalidPHC(t *testing.T) {
	h := NewArgon2idHasher()
	shortSalt := base64.RawStdEncoding.EncodeToString(make([]byte, 8))
	longSalt := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	shortKey := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	longKey := base64.RawStdEncoding.EncodeToString(make([]byte, 64))
	for _, encoded := range []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2i$v=19$m=19456,t=2,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=2,p=1$$$",
		"$argon2id$v=19$m=abc,t=2,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=2,p=1$" + validPHCSalt + "!!!" + "$" + validPHCKey,
		"$argon2id$v=19$t=2,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=2$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=2,p=1,m=19456$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,m=19456,t=2$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=2,p=1,k=32$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=4294967296,t=2,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=18446744073709551616,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=2,p=1$" + shortSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=2,p=1$" + longSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=2,p=1$" + validPHCSalt + "$" + shortKey,
		"$argon2id$v=19$m=19456,t=2,p=1$" + validPHCSalt + "$" + longKey,
	} {
		ok, err := h.Verify("x", encoded)
		if ok || err == nil {
			t.Fatalf("%q: got ok=%v err=%v", encoded, ok, err)
		}
	}
}

func TestArgon2idUnsupportedOrExcessiveParams(t *testing.T) {
	h := NewArgon2idHasher()
	cases := []string{
		"$argon2id$v=16$m=19456,t=2,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=999999,t=2,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=99,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=2,p=16$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=1024,t=2,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=1,p=1$" + validPHCSalt + "$" + validPHCKey,
		"$argon2id$v=19$m=19456,t=2,p=2$" + validPHCSalt + "$" + validPHCKey,
	}
	for _, encoded := range cases {
		ok, err := h.Verify("x", encoded)
		if ok || err == nil {
			t.Fatalf("%q: got ok=%v err=%v", encoded, ok, err)
		}
	}
}

func TestArgon2idDummyHash(t *testing.T) {
	h := NewArgon2idHasher()
	dummy := h.DummyHash()
	if dummy != dummyPHC {
		t.Fatalf("DummyHash must be the precomputed constant")
	}
	ok, err := h.Verify("paycore-dummy-password", dummy)
	if err != nil || !ok {
		t.Fatalf("dummy must be a valid argon2id hash: %v %v", ok, err)
	}
	ok, err = h.Verify("other", dummy)
	if err != nil || ok {
		t.Fatalf("dummy must reject other passwords: %v %v", ok, err)
	}
}
