package ledger

import (
	"errors"
	"fmt"
	"testing"

	"github.com/romangergovskiy/paycore/internal/platform/apperror"
)

func TestSentinelCodes(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{ErrInvalidJournal, "INVALID_JOURNAL"},
		{ErrInsufficientFunds, "INSUFFICIENT_FUNDS"},
		{ErrSystemAccountInsufficient, "SYSTEM_ACCOUNT_INSUFFICIENT"},
		{ErrAccountNotActive, "ACCOUNT_NOT_ACTIVE"},
		{ErrConflict, "CONFLICT"},
		{ErrLockProtocol, "LOCK_PROTOCOL_VIOLATION"},
	}
	for _, tc := range cases {
		wrapped := fmt.Errorf("post: %w", tc.err)
		if !errors.Is(wrapped, tc.err) {
			t.Fatalf("errors.Is(%v) = false", tc.err)
		}
		got := apperror.From(wrapped)
		if got.Code != tc.code {
			t.Fatalf("code = %s want %s", got.Code, tc.code)
		}
	}
}
