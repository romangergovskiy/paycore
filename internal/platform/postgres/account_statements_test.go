package postgres

import (
	"strings"
	"testing"
)

func TestSelectAccountStatementSQLIsUnlockedRead(t *testing.T) {
	sql := strings.ToUpper(selectAccountStatementSQL)
	for _, banned := range []string{
		"FOR UPDATE",
		"FOR NO KEY UPDATE",
		"LOCK_ACCOUNT_BALANCES",
		"POST_JOURNAL",
		"INSERT ",
		"UPDATE ",
		"DELETE ",
		"SUM(",
		"FLOAT",
		"DOUBLE",
		"NUMERIC",
		"OFFSET",
	} {
		if strings.Contains(sql, banned) {
			t.Fatalf("read SQL contains %q: %s", banned, selectAccountStatementSQL)
		}
	}
	if !strings.Contains(sql, "FROM LEDGER.LEDGER_ACCOUNTS") || !strings.Contains(sql, "JOIN LEDGER.LEDGER_ENTRIES") {
		t.Fatalf("want accounts+entries join: %s", selectAccountStatementSQL)
	}
	if !strings.Contains(sql, "KIND IN ('AVAILABLE', 'HOLD')") {
		t.Fatalf("want AVAILABLE+HOLD: %s", selectAccountStatementSQL)
	}
	if !strings.Contains(sql, "STATUS = 'POSTED'") {
		t.Fatalf("want POSTED: %s", selectAccountStatementSQL)
	}
	if !strings.Contains(sql, "ORDER BY E.CREATED_AT ASC, E.ID ASC") {
		t.Fatalf("want created_at, id: %s", selectAccountStatementSQL)
	}
	postedAt := strings.Index(sql, "STATUS = 'POSTED'")
	cursorAt := strings.Index(sql, "(E.CREATED_AT, E.ID) >")
	if postedAt < 0 || cursorAt < 0 || postedAt > cursorAt {
		t.Fatalf("POSTED must precede keyset: %s", selectAccountStatementSQL)
	}
}
