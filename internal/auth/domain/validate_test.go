package domain

import "testing"

func TestCanonicalEmail(t *testing.T) {
	got, err := CanonicalEmail("  A@B.C  ")
	if err != nil || got != "a@b.c" {
		t.Fatalf("got %q %v", got, err)
	}
	for _, raw := range []string{"", "ab", "no-at", "@x.com", "a@", "a@b@c"} {
		if _, err := CanonicalEmail(raw); err != ErrInvalidRequest {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("twelvechars!"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePassword("12345678901"); err != ErrInvalidRequest {
		t.Fatalf("11 runes: %v", err)
	}
	long := make([]rune, 129)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidatePassword(string(long)); err != ErrInvalidRequest {
		t.Fatalf("129 runes: %v", err)
	}
}

func TestPrincipalHas(t *testing.T) {
	p := Principal{Roles: []Role{RoleAdmin}}
	if p.IsActiveCustomer() || !p.Has(RoleAdmin) {
		t.Fatalf("%+v", p)
	}
	p.Roles = []Role{RoleCustomer, RoleAdmin}
	if !p.IsActiveCustomer() {
		t.Fatal("want customer")
	}
}
