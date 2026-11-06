package domain

import (
	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/pkg/money"
)

// ValidateOpen checks OpenCommand shape without database state.
// MVP product opens only RUB (D-30); pkg/money still accepts other ISO codes.
func ValidateOpen(cmd OpenCommand) error {
	if _, err := money.FromMinor(0, cmd.Currency); err != nil {
		return err
	}
	if cmd.Currency != money.RUB {
		return ErrCurrencyNotSupported
	}
	if cmd.AccountNumber == "" {
		return ErrInvalidAccount
	}
	if cmd.Reason == "" || cmd.ChangedBy == uuid.Nil {
		return ErrInvalidAccount
	}
	return validateOwnerPurpose(cmd.UserID, cmd.MerchantID, cmd.Purpose)
}

// ValidateOwner requires exactly one of user_id / merchant_id (SQL CHECK).
func ValidateOwner(userID, merchantID *uuid.UUID) error {
	u, m := OwnerSet(userID), OwnerSet(merchantID)
	if u == m {
		return ErrInvalidAccount
	}
	return nil
}

// ValidateStatusCommand requires a real account ID, a non-empty reason, and a real ChangedBy.
func ValidateStatusCommand(cmd StatusCommand) error {
	if cmd.ID == uuid.Nil || cmd.Reason == "" || cmd.ChangedBy == uuid.Nil {
		return ErrInvalidAccount
	}
	return nil
}

// ValidateTransition allows ACTIVE↔BLOCKED and ACTIVE|BLOCKED→CLOSED.
func ValidateTransition(from, to Status) error {
	switch to {
	case StatusBlocked:
		if from == StatusActive {
			return nil
		}
	case StatusActive:
		if from == StatusBlocked {
			return nil
		}
	case StatusClosed:
		if from == StatusActive || from == StatusBlocked {
			return nil
		}
	}
	return ErrInvalidState
}

func validateOwnerPurpose(userID, merchantID *uuid.UUID, purpose Purpose) error {
	if err := ValidateOwner(userID, merchantID); err != nil {
		return err
	}
	if OwnerSet(merchantID) {
		if purpose != PurposeMerchantSettlement {
			return ErrInvalidAccount
		}
		return nil
	}
	if purpose != PurposeMain && purpose != PurposeSavings {
		return ErrInvalidAccount
	}
	return nil
}
