package authcrypto

import (
	"strings"
	"testing"
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
	for _, encoded := range []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2i$v=19$m=19456,t=2,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$$$",
		"$argon2id$v=19$m=abc,t=2,p=1$cGF5Y29yZS1kdW1teS0xNg$jqoWTfRQ64xQMhdCUzMQAlFgF9MuueyMrkUvZ9p21gw",
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
		"$argon2id$v=16$m=19456,t=2,p=1$cGF5Y29yZS1kdW1teS0xNg$jqoWTfRQ64xQMhdCUzMQAlFgF9MuueyMrkUvZ9p21gw",
		"$argon2id$v=19$m=999999,t=2,p=1$cGF5Y29yZS1kdW1teS0xNg$jqoWTfRQ64xQMhdCUzMQAlFgF9MuueyMrkUvZ9p21gw",
		"$argon2id$v=19$m=19456,t=99,p=1$cGF5Y29yZS1kdW1teS0xNg$jqoWTfRQ64xQMhdCUzMQAlFgF9MuueyMrkUvZ9p21gw",
		"$argon2id$v=19$m=19456,t=2,p=16$cGF5Y29yZS1kdW1teS0xNg$jqoWTfRQ64xQMhdCUzMQAlFgF9MuueyMrkUvZ9p21gw",
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
