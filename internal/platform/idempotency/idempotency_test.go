package idempotency

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

func TestValidKey(t *testing.T) {
	if ValidKey("") || ValidKey(string(make([]byte, 256))) || ValidKey("bad\nkey") {
		t.Fatal("invalid keys accepted")
	}
	if !ValidKey("a") || !ValidKey("transfer-1") {
		t.Fatal("valid keys rejected")
	}
}

func TestScope(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	if !ValidScope(UserScope(id)) || !ValidScope(MerchantScope(id)) {
		t.Fatal("valid scopes rejected")
	}
	if ValidScope("apikey:abc") || ValidScope("user:") || ValidScope("user:not-a-uuid") {
		t.Fatal("invalid scopes accepted")
	}
}

func TestCanonicalJSONAndHash(t *testing.T) {
	a, err := CanonicalJSON([]byte(`{"b":1,"a":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalJSON([]byte(`{ "a" : "x", "b" : 1 }`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("%s vs %s", a, b)
	}
	if !bytes.Equal(a, []byte(`{"a":"x","b":1}`)) {
		t.Fatalf("canonical = %s", a)
	}
	// json.Number: large minor-unit amounts must not become float64.
	c, err := CanonicalJSON([]byte(`{"amount":10050}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(c) != `{"amount":10050}` {
		t.Fatalf("amount canonical = %s", c)
	}
	h1 := Hash("POST /api/v1/x", []string{"id"}, a)
	h2 := Hash("POST /api/v1/x", []string{"id"}, b)
	if !bytes.Equal(h1, h2) || len(h1) != 32 {
		t.Fatal("hash mismatch")
	}
	h3 := Hash("POST /api/v1/x", []string{"other"}, a)
	if bytes.Equal(h1, h3) {
		t.Fatal("path values ignored")
	}
}

func TestCanonicalJSONTrailing(t *testing.T) {
	ok, err := CanonicalJSON([]byte(`{"a":1}`))
	if err != nil || string(ok) != `{"a":1}` {
		t.Fatalf("%s %v", ok, err)
	}
	spaced, err := CanonicalJSON([]byte(`{"a":1}   `))
	if err != nil || string(spaced) != `{"a":1}` {
		t.Fatalf("trailing whitespace: %s %v", spaced, err)
	}
	if _, err := CanonicalJSON([]byte(`{"a":1}{"b":2}`)); err == nil {
		t.Fatal("two JSON values accepted")
	}
	if _, err := CanonicalJSON([]byte(`{"a":1} garbage`)); err == nil {
		t.Fatal("garbage after value accepted")
	}
	if _, err := CanonicalJSON([]byte(`{`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	empty, err := CanonicalJSON([]byte(`   `))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty body: %q %v", empty, err)
	}
	precise, err := CanonicalJSON([]byte(`{"amount":123456789012345}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(precise) != `{"amount":123456789012345}` {
		t.Fatalf("high-precision amount = %s", precise)
	}
}

func TestPersistable(t *testing.T) {
	if !Persistable(201) || !Persistable(422) || !Persistable(409) {
		t.Fatal("2xx/4xx must persist")
	}
	if Persistable(500) || Persistable(503) || Persistable(100) {
		t.Fatal("5xx must not persist")
	}
}
