package postgres

import "testing"

func TestRedactDSN(t *testing.T) {
	got := RedactDSN("postgres://paycore_app:s3cret@127.0.0.1:5432/paycore?sslmode=disable")
	want := "postgres://paycore_app:***@127.0.0.1:5432/paycore?sslmode=disable"
	if got != want {
		t.Errorf("RedactDSN = %q, want %q", got, want)
	}
	if RedactDSN("::::") == "" {
		t.Error("empty redact")
	}
}

func TestParseDSN(t *testing.T) {
	u, err := parseDSN("postgres://paycore_owner:x@localhost:5432/paycore")
	if err != nil {
		t.Fatal(err)
	}
	if dbName(u) != "paycore" || u.User.Username() != "paycore_owner" {
		t.Errorf("parsed %+v", u)
	}
	if _, err := parseDSN("http://localhost/paycore"); err == nil {
		t.Error("expected scheme error")
	}
}
