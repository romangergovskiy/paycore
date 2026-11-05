package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/pkg/money"
)

func TestValidateOpen(t *testing.T) {
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	merchant := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-000000000002")

	okCustomer := OpenCommand{
		AccountNumber: "PC-TEST-1",
		UserID:        &user,
		Purpose:       PurposeMain,
		Currency:      money.RUB,
	}
	okSavings := okCustomer
	okSavings.Purpose = PurposeSavings
	okMerchant := OpenCommand{
		AccountNumber: "PC-TEST-M",
		MerchantID:    &merchant,
		Purpose:       PurposeMerchantSettlement,
		Currency:      money.RUB,
	}

	tests := []struct {
		name string
		cmd  OpenCommand
		want error
	}{
		{name: "CUSTOMER MAIN RUB", cmd: okCustomer},
		{name: "CUSTOMER SAVINGS RUB", cmd: okSavings},
		{name: "MERCHANT SETTLEMENT RUB", cmd: okMerchant},
		{
			name: "USD forbidden",
			cmd:  withCur(okCustomer, money.Currency("USD")),
			want: ErrCurrencyNotSupported,
		},
		{
			name: "EUR forbidden",
			cmd:  withCur(okCustomer, money.Currency("EUR")),
			want: ErrCurrencyNotSupported,
		},
		{
			name: "empty currency",
			cmd:  withCur(okCustomer, ""),
			want: money.ErrInvalidCurrency,
		},
		{
			name: "lowercase currency",
			cmd:  withCur(okCustomer, "rub"),
			want: money.ErrInvalidCurrency,
		},
		{
			name: "short currency",
			cmd:  withCur(okCustomer, "RU"),
			want: money.ErrInvalidCurrency,
		},
		{
			name: "CUSTOMER merchant purpose",
			cmd:  withPurpose(okCustomer, PurposeMerchantSettlement),
			want: ErrInvalidAccount,
		},
		{
			name: "MERCHANT main purpose",
			cmd:  withPurpose(okMerchant, PurposeMain),
			want: ErrInvalidAccount,
		},
		{
			name: "both owners",
			cmd: OpenCommand{
				AccountNumber: "PC-BOTH",
				UserID:        &user,
				MerchantID:    &merchant,
				Purpose:       PurposeMain,
				Currency:      money.RUB,
			},
			want: ErrInvalidAccount,
		},
		{
			name: "no owner",
			cmd: OpenCommand{
				AccountNumber: "PC-NONE",
				Purpose:       PurposeMain,
				Currency:      money.RUB,
			},
			want: ErrInvalidAccount,
		},
		{
			name: "nil UUID owner",
			cmd: OpenCommand{
				AccountNumber: "PC-NIL",
				UserID:        &uuid.Nil,
				Purpose:       PurposeMain,
				Currency:      money.RUB,
			},
			want: ErrInvalidAccount,
		},
		{
			name: "empty account number",
			cmd: OpenCommand{
				UserID:   &user,
				Purpose:  PurposeMain,
				Currency: money.RUB,
			},
			want: ErrInvalidAccount,
		},
		{
			name: "unknown purpose",
			cmd:  withPurpose(okCustomer, "CHECKING"),
			want: ErrInvalidAccount,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOpen(tc.cmd)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidateOpen: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestValidateStatusCommand(t *testing.T) {
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	id := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000009")
	ok := StatusCommand{ID: id, Reason: "review", ChangedBy: actor}
	if err := ValidateStatusCommand(ok); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStatusCommand(StatusCommand{ID: id, ChangedBy: actor}); !errors.Is(err, ErrInvalidAccount) {
		t.Fatalf("empty reason: %v", err)
	}
	if err := ValidateStatusCommand(StatusCommand{ID: id, Reason: "review"}); !errors.Is(err, ErrInvalidAccount) {
		t.Fatalf("nil actor: %v", err)
	}
}

func TestValidateTransition(t *testing.T) {
	tests := []struct {
		from, to Status
		want     error
	}{
		{StatusActive, StatusBlocked, nil},
		{StatusBlocked, StatusActive, nil},
		{StatusActive, StatusClosed, nil},
		{StatusBlocked, StatusClosed, nil},
		{StatusActive, StatusActive, ErrInvalidState},
		{StatusBlocked, StatusBlocked, ErrInvalidState},
		{StatusClosed, StatusActive, ErrInvalidState},
		{StatusClosed, StatusBlocked, ErrInvalidState},
		{StatusClosed, StatusClosed, ErrInvalidState},
	}
	for _, tc := range tests {
		err := ValidateTransition(tc.from, tc.to)
		if tc.want == nil {
			if err != nil {
				t.Fatalf("%s→%s: %v", tc.from, tc.to, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s→%s got %v want %v", tc.from, tc.to, err, tc.want)
		}
	}
}

func TestStatusConstants(t *testing.T) {
	for _, st := range []Status{StatusActive, StatusBlocked, StatusClosed} {
		if st == "" {
			t.Fatalf("empty status")
		}
	}
	if StatusActive == StatusBlocked || StatusBlocked == StatusClosed {
		t.Fatal("statuses must differ")
	}
}

func withCur(cmd OpenCommand, c money.Currency) OpenCommand {
	cmd.Currency = c
	return cmd
}

func withPurpose(cmd OpenCommand, p Purpose) OpenCommand {
	cmd.Purpose = p
	return cmd
}
