package money

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFromMinor(t *testing.T) {
	m, err := FromMinor(10050, RUB)
	if err != nil {
		t.Fatal(err)
	}
	if m.Amount() != 10050 || m.Currency() != RUB {
		t.Fatalf("got %+v", m)
	}

	usd, err := FromMinor(1, Currency("USD"))
	if err != nil {
		t.Fatal(err)
	}
	if usd.Currency() != "USD" || usd.Amount() != 1 {
		t.Fatalf("USD: %+v", usd)
	}
	eur, err := FromMinor(0, Currency("EUR"))
	if err != nil {
		t.Fatal(err)
	}
	if !eur.IsZero() || eur.Currency() != "EUR" {
		t.Fatalf("EUR zero: %+v", eur)
	}

	neg, err := FromMinor(-1, RUB)
	if err != nil {
		t.Fatal(err)
	}
	if !neg.IsNegative() || neg.Amount() != -1 {
		t.Fatalf("negative: %+v", neg)
	}

	for _, cur := range []Currency{"", "RU", "RUBX", "rub", "RU1", "RÜB"} {
		if _, err := FromMinor(0, cur); !errors.Is(err, ErrInvalidCurrency) {
			t.Fatalf("currency %q: %v", cur, err)
		}
	}
}

func TestAddSub(t *testing.T) {
	a := must(t, 100, RUB)
	b := must(t, 40, RUB)

	sum, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Amount() != 140 || sum.Currency() != RUB {
		t.Fatalf("add = %+v", sum)
	}

	diff, err := a.Sub(b)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Amount() != 60 {
		t.Fatalf("sub = %+v", diff)
	}

	neg, err := b.Sub(a)
	if err != nil {
		t.Fatal(err)
	}
	if !neg.IsNegative() || neg.Amount() != -60 {
		t.Fatalf("sub negative = %+v", neg)
	}

	z, err := a.Add(must(t, 0, RUB))
	if err != nil || z.Amount() != 100 || !must(t, 0, RUB).IsZero() {
		t.Fatalf("zero add: %+v %v", z, err)
	}
}

func TestOverflow(t *testing.T) {
	max := must(t, math.MaxInt64, RUB)
	one := must(t, 1, RUB)
	if _, err := max.Add(one); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MaxInt64+1: %v", err)
	}
	if _, err := max.Add(max); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MaxInt64+MaxInt64: %v", err)
	}

	min := must(t, math.MinInt64, RUB)
	if _, err := min.Sub(one); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MinInt64-1: %v", err)
	}
	if _, err := min.Add(must(t, -1, RUB)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MinInt64+(-1): %v", err)
	}
	if _, err := must(t, 0, RUB).Sub(min); !errors.Is(err, ErrOverflow) {
		t.Fatalf("0-MinInt64: %v", err)
	}

	got, err := min.Sub(min)
	if err != nil || got.Amount() != 0 {
		t.Fatalf("MinInt64-MinInt64: %+v %v", got, err)
	}
	got, err = must(t, -1, RUB).Sub(min)
	if err != nil || got.Amount() != math.MaxInt64 {
		t.Fatalf("-1-MinInt64: %+v %v", got, err)
	}
}

func TestSameCurrency(t *testing.T) {
	rub := must(t, 10, RUB)
	usd := must(t, 10, Currency("USD"))

	if _, err := rub.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("add: %v", err)
	}
	if _, err := rub.Sub(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("sub: %v", err)
	}
	cmp, err := rub.Compare(usd)
	if !errors.Is(err, ErrCurrencyMismatch) || cmp != 0 {
		t.Fatalf("compare: %d %v", cmp, err)
	}

	zero := Money{}
	if _, err := rub.Add(zero); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("zero value: %v", err)
	}
}

func TestCompareIsZeroNegative(t *testing.T) {
	a := must(t, 5, RUB)
	b := must(t, 5, RUB)
	c := must(t, 7, RUB)

	n, err := a.Compare(b)
	if err != nil || n != 0 {
		t.Fatalf("equal: %d %v", n, err)
	}
	n, err = a.Compare(c)
	if err != nil || n != -1 {
		t.Fatalf("less: %d %v", n, err)
	}
	n, err = c.Compare(a)
	if err != nil || n != 1 {
		t.Fatalf("greater: %d %v", n, err)
	}

	if !must(t, 0, RUB).IsZero() || must(t, 1, RUB).IsZero() || must(t, -1, RUB).IsZero() {
		t.Fatal("IsZero")
	}
	if must(t, 0, RUB).IsNegative() || must(t, 1, RUB).IsNegative() || !must(t, -1, RUB).IsNegative() {
		t.Fatal("IsNegative")
	}
}

func TestNoFloat(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if id.Name == "float32" || id.Name == "float64" {
					t.Errorf("%s uses %s", name, id.Name)
				}
				return true
			})
		}
	}
}

func must(t *testing.T, amount int64, cur Currency) Money {
	t.Helper()
	m, err := FromMinor(amount, cur)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
