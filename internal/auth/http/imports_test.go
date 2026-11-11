package authhttp

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHTTPDoesNotImportPgx(t *testing.T) {
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
	banned := []string{
		`"github.com/jackc/pgx/v5"`,
		`"github.com/jackc/pgx/v5/pgconn"`,
		`"github.com/romangergovskiy/paycore/internal/platform/postgres"`,
	}
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			for _, spec := range f.Imports {
				for _, b := range banned {
					if spec.Path.Value == b {
						t.Fatalf("%s imports %s", name, spec.Path.Value)
					}
				}
			}
		}
	}
}
