package postgres

import (
	"strings"
	"testing"
)

func TestSelectAccountBalanceSQLIsUnlockedRead(t *testing.T) {
	sql := strings.ToUpper(selectAccountBalanceSQL)
	for _, banned := range []string{
		"FOR UPDATE",
		"FOR NO KEY UPDATE",
		"LOCK_ACCOUNT_BALANCES",
		"POST_JOURNAL",
		"INSERT ",
		"UPDATE ",
		"DELETE ",
	} {
		if strings.Contains(sql, banned) {
			t.Fatalf("read SQL contains %q: %s", banned, selectAccountBalanceSQL)
		}
	}
	if !strings.Contains(sql, "FROM LEDGER.ACCOUNT_BALANCES") {
		t.Fatalf("want ledger.account_balances: %s", selectAccountBalanceSQL)
	}
}
