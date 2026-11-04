package ledger

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSentinelCodes(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{ErrInvalidJournal, CodeInvalidJournal},
		{ErrInsufficientFunds, CodeInsufficientFunds},
		{ErrSystemAccountInsufficient, CodeSystemAccountInsufficient},
		{ErrAccountNotActive, CodeAccountNotActive},
		{ErrConflict, CodeConflict},
		{ErrLockProtocol, CodeLockProtocol},
	}
	for _, tc := range cases {
		wrapped := fmt.Errorf("post: %w", tc.err)
		if !errors.Is(wrapped, tc.err) {
			t.Fatalf("errors.Is(%v) = false", tc.err)
		}
		var got *Error
		if !errors.As(wrapped, &got) || got.Code != tc.code {
			t.Fatalf("code = %v want %s", got, tc.code)
		}
	}
	if errors.Is(ErrInsufficientFunds, ErrSystemAccountInsufficient) {
		t.Fatal("PC402 codes must not match each other")
	}
}

func TestDomainDoesNotImportAppError(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			for _, spec := range f.Imports {
				path := spec.Path.Value
				for _, banned := range []string{
					`"github.com/romangergovskiy/paycore/internal/platform/apperror"`,
					`"github.com/jackc/pgx/v5"`,
					`"github.com/jackc/pgx/v5/pgconn"`,
					`"net/http"`,
				} {
					if path == banned {
						t.Fatalf("%s imports %s", name, path)
					}
				}
			}
		}
	}
}
