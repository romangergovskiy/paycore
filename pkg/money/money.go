// Package money is integer minor-unit amounts with a currency code.
//
// Amounts are int64. There is no float, formatting, rounding, or FX.
// MVP product currency is RUB ([D-30]); the type accepts any ISO-like
// three-letter code so a second currency does not require a new type.
package money

import (
	"errors"
	"math"
)

type Currency string

// RUB is the only currency the MVP product opens. Other codes are valid
// Money values; product policy (CURRENCY_NOT_SUPPORTED) lives outside this package.
const RUB Currency = "RUB"

var (
	ErrOverflow         = errors.New("money: integer overflow")
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrInvalidCurrency  = errors.New("money: invalid currency")
)

// Money is a signed amount in minor units of one currency.
// Negative values are allowed (a subtraction result); ledger entries stay > 0.
type Money struct {
	amount   int64
	currency Currency
}

// FromMinor builds Money from already-quantized minor units (kopecks for RUB).
func FromMinor(amount int64, currency Currency) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, ErrInvalidCurrency
	}
	return Money{amount: amount, currency: currency}, nil
}

func (m Money) Amount() int64      { return m.amount }
func (m Money) Currency() Currency { return m.currency }

func (m Money) IsZero() bool     { return m.amount == 0 }
func (m Money) IsNegative() bool { return m.amount < 0 }

func (m Money) Add(o Money) (Money, error) {
	if err := sameCurrency(m, o); err != nil {
		return Money{}, err
	}
	sum, ok := addInt64(m.amount, o.amount)
	if !ok {
		return Money{}, ErrOverflow
	}
	return Money{amount: sum, currency: m.currency}, nil
}

func (m Money) Sub(o Money) (Money, error) {
	if err := sameCurrency(m, o); err != nil {
		return Money{}, err
	}
	diff, ok := subInt64(m.amount, o.amount)
	if !ok {
		return Money{}, ErrOverflow
	}
	return Money{amount: diff, currency: m.currency}, nil
}

// Compare returns -1, 0, or 1 if m <, ==, or > o. Different currencies error;
// they are not ordered and not treated as unequal.
func (m Money) Compare(o Money) (int, error) {
	if err := sameCurrency(m, o); err != nil {
		return 0, err
	}
	switch {
	case m.amount < o.amount:
		return -1, nil
	case m.amount > o.amount:
		return 1, nil
	default:
		return 0, nil
	}
}

func sameCurrency(a, b Money) error {
	if !validCurrency(a.currency) || !validCurrency(b.currency) {
		return ErrInvalidCurrency
	}
	if a.currency != b.currency {
		return ErrCurrencyMismatch
	}
	return nil
}

func validCurrency(c Currency) bool {
	if len(c) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return false
		}
	}
	return true
}

func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

func subInt64(a, b int64) (int64, bool) {
	if b < 0 && a > math.MaxInt64+b {
		return 0, false
	}
	if b > 0 && a < math.MinInt64+b {
		return 0, false
	}
	return a - b, true
}
