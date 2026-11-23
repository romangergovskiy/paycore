package prclass_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func classifyStdin(t *testing.T, stdin string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	script := filepath.Join(root, ".github", "scripts", "classify-pr-paths.sh")
	cmd := exec.Command("bash", script)
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("classify stdin %q: %v\n%s", stdin, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func classify(t *testing.T, files ...string) string {
	t.Helper()
	if len(files) == 0 {
		return classifyStdin(t, "")
	}
	return classifyStdin(t, strings.Join(files, "\n")+"\n")
}

func TestClassifyPRPaths(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{"ordinary docs", []string{"README.md"}, "docs-class"},
		{"ordinary tests", []string{"internal/platform/apperror/apperror_test.go"}, "tests-class"},
		{"ordinary config tests", []string{"internal/platform/config/config_test.go"}, "tests-class"},
		{"ledger", []string{"internal/ledger/journal.go"}, "financial"},
		{"ledger tests", []string{"internal/ledger/journal_test.go"}, "financial"},
		{"CI", []string{".github/workflows/ci.yml"}, "financial"},
		{"migrations", []string{"migrations/00003_ledger.sql"}, "financial"},
		{"financial docs", []string{"docs/architecture/financial-invariants-testing.md"}, "financial"},
		{"cursor environment", []string{".cursor/environment.json"}, "financial"},
		{"AGENTS.md", []string{"AGENTS.md"}, "financial"},
		{"mixed PR", []string{"README.md", "internal/ledger/journal.go"}, "financial"},
		{"post_journal tests", []string{"internal/platform/postgres/post_journal_test.go"}, "financial"},
		{"account balances tests", []string{"internal/platform/postgres/account_balances_test.go"}, "financial"},
		{"accounts http pg tests", []string{"internal/platform/postgres/accounts_http_pg_test.go"}, "financial"},
		{"accounts uow pg tests", []string{"internal/platform/postgres/accounts_uow_pg_test.go"}, "financial"},
		{"idempotency tests", []string{"internal/platform/idempotency/idempotency_test.go"}, "financial"},
		{"idempotency pg tests", []string{"internal/platform/postgres/idempotency_pg_test.go"}, "financial"},
		{"outbox pg tests", []string{"internal/platform/postgres/outbox_pg_test.go"}, "financial"},
		{"app tests", []string{"internal/app/app_test.go"}, "financial"},
		{"empty input", nil, "financial"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(t, tc.files...)
			if got != tc.want {
				t.Fatalf("got %q want %q files=%v", got, tc.want, tc.files)
			}
		})
	}
}
