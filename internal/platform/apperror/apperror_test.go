package apperror

import (
	"errors"
	"fmt"
	"testing"
)

var errNotActive = New(KindUnprocessable, "ACCOUNT_NOT_ACTIVE", "Account is not active.")

func TestFromKeepsAppError(t *testing.T) {
	cause := errors.New("row locked")
	err := fmt.Errorf("transfer: %w", Wrap(cause, KindConflict, "REQUEST_IN_PROGRESS", "Request is in progress."))

	got := From(err)
	if got.Kind != KindConflict || got.Code != "REQUEST_IN_PROGRESS" {
		t.Fatalf("From = %+v", got)
	}
	if !errors.Is(err, cause) {
		t.Error("cause is not reachable through errors.Is")
	}
}

func TestFromWrapsUnknownErrorAsInternal(t *testing.T) {
	cause := errors.New("connection reset")
	got := From(cause)
	if got.Kind != KindInternal || got.Code != CodeInternal {
		t.Fatalf("From = %+v", got)
	}
	if got.PublicMessage() != internalMessage {
		t.Errorf("public message leaks details: %q", got.PublicMessage())
	}
	if !errors.Is(got, cause) {
		t.Error("cause lost")
	}
}

func TestFromNil(t *testing.T) {
	if From(nil) != nil {
		t.Error("From(nil) != nil")
	}
}

func TestIsMatchesByCode(t *testing.T) {
	err := fmt.Errorf("refund: %w", Wrap(errors.New("db"), KindUnprocessable, "ACCOUNT_NOT_ACTIVE", "Merchant account is blocked."))
	if !errors.Is(err, errNotActive) {
		t.Error("errors.Is does not match an Error with the same code")
	}
	if errors.Is(err, New(KindUnprocessable, "INSUFFICIENT_FUNDS", "")) {
		t.Error("errors.Is matches a different code")
	}
}

func TestPublicMessageHidesInternalMessage(t *testing.T) {
	e := New(KindInternal, CodeInternal, "pgx: password authentication failed")
	if e.PublicMessage() != internalMessage {
		t.Errorf("PublicMessage = %q", e.PublicMessage())
	}
}
