package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	sys5900 = "00000000-0000-4000-8000-000000005900"
	sys4200 = "00000000-0000-4000-8000-000000004200"
)

type idgen struct {
	mu sync.Mutex
	n  int
}

func (g *idgen) next() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", g.n)
}

type pjEntry struct {
	ID              string `json:"id"`
	LedgerAccountID string `json:"ledger_account_id"`
	EntryType       string `json:"entry_type"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
}

type pjRow struct {
	JournalID       string
	AlreadyPosted   bool
	EntryID         string
	LedgerAccountID string
	EntryType       string
	Amount          int64
	Currency        string
	AccountVersion  *int64
	BalanceAfter    *int64
}

func TestPostJournalGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	c := startCluster(t)
	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}

	owner := mustConnect(t, ctx, c.owner)
	defer owner.Close(ctx)
	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)
	ro := mustConnect(t, ctx, c.readonly)
	defer ro.Close(ctx)
	ids := &idgen{}

	t.Run("function owner execute and search_path", func(t *testing.T) {
		var fnOwner string
		var definer bool
		var cfg []string
		if err := owner.QueryRow(ctx, `
			SELECT r.rolname, p.prosecdef, coalesce(p.proconfig, '{}')
			  FROM pg_proc p
			  JOIN pg_namespace n ON n.oid = p.pronamespace
			  JOIN pg_roles r ON r.oid = p.proowner
			 WHERE n.nspname = 'ledger' AND p.proname = 'post_journal'`).Scan(&fnOwner, &definer, &cfg); err != nil {
			t.Fatal(err)
		}
		if fnOwner != RoleLedger {
			t.Fatalf("post_journal owner = %s", fnOwner)
		}
		if !definer {
			t.Fatal("post_journal is not SECURITY DEFINER")
		}
		found := false
		for _, s := range cfg {
			if s == "search_path=pg_catalog, ledger, pg_temp" || s == "search_path=pg_catalog,ledger,pg_temp" {
				found = true
			}
		}
		if !found {
			t.Fatalf("search_path = %v", cfg)
		}

		var appExec, roExec, pubExec bool
		if err := owner.QueryRow(ctx, `
			SELECT has_function_privilege('paycore_app', p.oid, 'EXECUTE'),
			       has_function_privilege('paycore_readonly', p.oid, 'EXECUTE'),
			       has_function_privilege('public', p.oid, 'EXECUTE')
			  FROM pg_proc p
			  JOIN pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = 'ledger' AND p.proname = 'post_journal'`).Scan(&appExec, &roExec, &pubExec); err != nil {
			t.Fatal(err)
		}
		if !appExec {
			t.Fatal("paycore_app lacks EXECUTE on post_journal")
		}
		if roExec {
			t.Fatal("paycore_readonly has EXECUTE on post_journal")
		}
		if pubExec {
			t.Fatal("PUBLIC has EXECUTE on post_journal")
		}

		var formOwner string
		var formPub bool
		if err := owner.QueryRow(ctx, `
			SELECT r.rolname, has_function_privilege('public', p.oid, 'EXECUTE')
			  FROM pg_proc p
			  JOIN pg_namespace n ON n.oid = p.pronamespace
			  JOIN pg_roles r ON r.oid = p.proowner
			 WHERE n.nspname = 'ledger' AND p.proname = 'assert_journal_form'`).Scan(&formOwner, &formPub); err != nil {
			t.Fatal(err)
		}
		if formOwner != RoleLedger {
			t.Fatalf("assert_journal_form owner = %s", formOwner)
		}
		if formPub {
			t.Fatal("PUBLIC has EXECUTE on assert_journal_form")
		}

		var nologin bool
		if err := owner.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = 'paycore_ledger'`).Scan(&nologin); err != nil {
			t.Fatal(err)
		}
		if nologin {
			t.Fatal("paycore_ledger can log in")
		}
	})

	t.Run("readonly cannot execute", func(t *testing.T) {
		_, err := ro.Exec(ctx, `SELECT ledger.lock_account_balances(ARRAY[]::uuid[])`)
		if pgCode(err) == "" && err == nil {
			t.Fatal("readonly executed lock_account_balances")
		}
	})

	cust := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-1")
	recv := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-2")

	t.Run("lock_account_balances sorts ids", func(t *testing.T) {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := shareAccounts(ctx, tx, cust.accountID, recv.accountID); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx, `SELECT account_id::text FROM ledger.lock_account_balances(ARRAY[$1,$2]::uuid[])`, recv.accountID, cust.accountID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		rows.Close()
		if len(got) != 2 || got[0] > got[1] {
			t.Fatalf("lock order = %v", got)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("success SIM_TOPUP projections", func(t *testing.T) {
		jid := ids.next()
		rows, err := postJournal(ctx, app, []string{cust.accountID}, postArgs{
			journalID:     jid,
			operation:     "SIM_TOPUP",
			referenceType: "SIM_TOPUP",
			referenceID:   ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 10000, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: cust.avail, EntryType: "CREDIT", Amount: 10000, Currency: "RUB"},
			},
		})
		if err != nil {
			t.Fatalf("SIM_TOPUP: %v", err)
		}
		if len(rows) != 2 || rows[0].AlreadyPosted {
			t.Fatalf("rows = %+v", rows)
		}
		var avail, reserved, version int64
		if err := app.QueryRow(ctx, `SELECT available_amount, reserved_amount, version FROM ledger.account_balances WHERE account_id = $1`, cust.accountID).Scan(&avail, &reserved, &version); err != nil {
			t.Fatal(err)
		}
		if avail != 10000 || reserved != 0 || version != 1 {
			t.Fatalf("projection avail=%d reserved=%d version=%d", avail, reserved, version)
		}
		var bucketSum int64
		if err := app.QueryRow(ctx, `SELECT coalesce(sum(balance),0) FROM ledger.system_balance_buckets WHERE ledger_account_id = $1`, sys1100).Scan(&bucketSum); err != nil {
			t.Fatal(err)
		}
		if bucketSum != 10000 {
			t.Fatalf("1100 buckets = %d", bucketSum)
		}
		var recomputed int64
		if err := app.QueryRow(ctx, `
			SELECT coalesce(sum(CASE WHEN e.entry_type = a.normal_balance THEN e.amount ELSE -e.amount END),0)
			  FROM ledger.ledger_entries e
			  JOIN ledger.ledger_accounts a ON a.id = e.ledger_account_id
			 WHERE e.ledger_account_id = $1`, cust.avail).Scan(&recomputed); err != nil {
			t.Fatal(err)
		}
		if recomputed != 10000 {
			t.Fatalf("recomputed available ledger = %d", recomputed)
		}
		var status string
		var updatedAt time.Time
		if err := app.QueryRow(ctx, `SELECT status, updated_at FROM accounts.accounts WHERE id = $1`, cust.accountID).Scan(&status, &updatedAt); err != nil {
			t.Fatal(err)
		}
		if status != "ACTIVE" {
			t.Fatalf("account status changed to %s", status)
		}
		for _, row := range rows {
			if row.LedgerAccountID == cust.avail {
				if row.AccountVersion == nil || *row.AccountVersion != 1 || row.BalanceAfter == nil || *row.BalanceAfter != 10000 {
					t.Fatalf("customer entry stamps = %+v", row)
				}
			}
			if row.LedgerAccountID == sys1100 && (row.AccountVersion != nil || row.BalanceAfter != nil) {
				t.Fatalf("system entry must not stamp version: %+v", row)
			}
		}
	})

	t.Run("rejections PC400", func(t *testing.T) {
		cases := []struct {
			name    string
			entries []pjEntry
		}{
			{"empty", nil},
			{"one entry", []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 1, Currency: "RUB"},
			}},
			{"unbalanced", []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 100, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: cust.avail, EntryType: "CREDIT", Amount: 50, Currency: "RUB"},
			}},
			{"zero amount", []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 0, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: cust.avail, EntryType: "CREDIT", Amount: 0, Currency: "RUB"},
			}},
			{"negative amount", []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: -1, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: cust.avail, EntryType: "CREDIT", Amount: -1, Currency: "RUB"},
			}},
			{"over max", []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 100000000000001, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: cust.avail, EntryType: "CREDIT", Amount: 100000000000001, Currency: "RUB"},
			}},
			{"duplicate account", []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 10, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "CREDIT", Amount: 10, Currency: "RUB"},
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				before := readState(t, ctx, app, cust.accountID)
				_, err := postJournal(ctx, app, []string{cust.accountID}, postArgs{
					journalID:     ids.next(),
					operation:     "SIM_TOPUP",
					referenceType: "SIM_TOPUP",
					referenceID:   ids.next(),
					entries:       tc.entries,
				})
				requireCode(t, err, "PC400")
				requireState(t, readState(t, ctx, app, cust.accountID), before)
			})
		}

		t.Run("foreign currency", func(t *testing.T) {
			before := readState(t, ctx, app, cust.accountID)
			_, err := postJournal(ctx, app, []string{cust.accountID}, postArgs{
				journalID:     ids.next(),
				operation:     "SIM_TOPUP",
				referenceType: "SIM_TOPUP",
				referenceID:   ids.next(),
				entries: []pjEntry{
					{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 10, Currency: "USD"},
					{ID: ids.next(), LedgerAccountID: cust.avail, EntryType: "CREDIT", Amount: 10, Currency: "USD"},
				},
			})
			requireCode(t, err, "PC400")
			requireState(t, readState(t, ctx, app, cust.accountID), before)
		})
	})

	t.Run("blocked and closed PC403", func(t *testing.T) {
		blocked := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-BLK")
		if _, err := app.Exec(ctx, `UPDATE accounts.accounts SET status = 'BLOCKED', version = version + 1 WHERE id = $1`, blocked.accountID); err != nil {
			t.Fatal(err)
		}
		blkBefore := readState(t, ctx, app, blocked.accountID)
		_, err := postJournal(ctx, app, []string{blocked.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "SIM_TOPUP",
			referenceType: "SIM_TOPUP",
			referenceID:   ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 5, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: blocked.avail, EntryType: "CREDIT", Amount: 5, Currency: "RUB"},
			},
		})
		requireCode(t, err, "PC403")
		requireState(t, readState(t, ctx, app, blocked.accountID), blkBefore)

		closed := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-CLS")
		if _, err := app.Exec(ctx, `UPDATE accounts.accounts SET status = 'CLOSED', closed_at = now(), version = version + 1 WHERE id = $1`, closed.accountID); err != nil {
			t.Fatal(err)
		}
		clsBefore := readState(t, ctx, app, closed.accountID)
		_, err = postJournal(ctx, app, []string{closed.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "SIM_TOPUP",
			referenceType: "SIM_TOPUP",
			referenceID:   ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 5, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: closed.avail, EntryType: "CREDIT", Amount: 5, Currency: "RUB"},
			},
		})
		requireCode(t, err, "PC403")
		requireState(t, readState(t, ctx, app, closed.accountID), clsBefore)
	})

	t.Run("insufficient funds PC402", func(t *testing.T) {
		before := readState(t, ctx, app, cust.accountID)
		recvBefore := readState(t, ctx, app, recv.accountID)
		_, err := postJournal(ctx, app, []string{cust.accountID, recv.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "TRANSFER",
			referenceType: "TRANSFER",
			referenceID:   ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: cust.avail, EntryType: "DEBIT", Amount: 1_000_000, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: recv.avail, EntryType: "CREDIT", Amount: 1_000_000, Currency: "RUB"},
			},
		})
		requireCode(t, err, "PC402")
		if got := pgMsg(err); got != "" && got != "INSUFFICIENT_FUNDS" {
			t.Logf("PC402 message = %s", got)
		}
		requireState(t, readState(t, ctx, app, cust.accountID), before)
		requireState(t, readState(t, ctx, app, recv.accountID), recvBefore)

		sysBefore := readState(t, ctx, app, cust.accountID)
		_, err = postJournal(ctx, app, nil, postArgs{
			journalID:     ids.next(),
			operation:     "ADJUSTMENT",
			referenceType: "ADJUSTMENT",
			referenceID:   ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys4100, EntryType: "DEBIT", Amount: 1, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: sys5900, EntryType: "CREDIT", Amount: 1, Currency: "RUB"},
			},
		})
		requireCode(t, err, "PC402")
		if got := pgMsg(err); got != "SYSTEM_ACCOUNT_INSUFFICIENT" {
			t.Fatalf("system PC402 message = %q", got)
		}
		requireState(t, readState(t, ctx, app, cust.accountID), sysBefore)
	})

	t.Run("idempotent replay and conflict", func(t *testing.T) {
		ref := ids.next()
		e1, e2 := ids.next(), ids.next()
		entries := []pjEntry{
			{ID: e1, LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 250, Currency: "RUB"},
			{ID: e2, LedgerAccountID: cust.avail, EntryType: "CREDIT", Amount: 250, Currency: "RUB"},
		}
		first, err := postJournal(ctx, app, []string{cust.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "SIM_TOPUP",
			referenceType: "SIM_TOPUP",
			referenceID:   ref,
			entries:       entries,
		})
		if err != nil {
			t.Fatal(err)
		}
		var versionBefore int64
		if err := app.QueryRow(ctx, `SELECT version FROM ledger.account_balances WHERE account_id = $1`, cust.accountID).Scan(&versionBefore); err != nil {
			t.Fatal(err)
		}
		replay, err := postJournal(ctx, app, []string{cust.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "SIM_TOPUP",
			referenceType: "SIM_TOPUP",
			referenceID:   ref,
			entries:       entries,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !replay[0].AlreadyPosted || replay[0].JournalID != first[0].JournalID {
			t.Fatalf("replay = %+v first = %+v", replay, first)
		}
		var versionAfter, journals int64
		if err := app.QueryRow(ctx, `SELECT version FROM ledger.account_balances WHERE account_id = $1`, cust.accountID).Scan(&versionAfter); err != nil {
			t.Fatal(err)
		}
		if versionAfter != versionBefore {
			t.Fatalf("replay mutated version %d → %d", versionBefore, versionAfter)
		}
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions WHERE reference_id = $1`, ref).Scan(&journals); err != nil {
			t.Fatal(err)
		}
		if journals != 1 {
			t.Fatalf("journals for ref = %d", journals)
		}

		_, err = postJournal(ctx, app, []string{cust.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "SIM_TOPUP",
			referenceType: "SIM_TOPUP",
			referenceID:   ref,
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 251, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: cust.avail, EntryType: "CREDIT", Amount: 251, Currency: "RUB"},
			},
		})
		requireCode(t, err, "PC409")
	})

	t.Run("immutable after post", func(t *testing.T) {
		var jid string
		if err := app.QueryRow(ctx, `SELECT id::text FROM ledger.journal_transactions WHERE operation = 'SIM_TOPUP' LIMIT 1`).Scan(&jid); err != nil {
			t.Fatal(err)
		}
		mustFail(t, ctx, owner, `UPDATE ledger.journal_transactions SET description = 'x' WHERE id = $1`, jid)
		mustFail(t, ctx, owner, `DELETE FROM ledger.ledger_entries WHERE journal_transaction_id = $1`, jid)
		mustFail(t, ctx, app, `UPDATE ledger.journal_transactions SET description = 'x' WHERE id = $1`, jid)
	})

	t.Run("failed call rolls back", func(t *testing.T) {
		var journalsBefore, availBefore int64
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&journalsBefore); err != nil {
			t.Fatal(err)
		}
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, cust.accountID).Scan(&availBefore); err != nil {
			t.Fatal(err)
		}
		_, err := postJournal(ctx, app, []string{cust.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "SIM_TOPUP",
			referenceType: "SIM_TOPUP",
			referenceID:   ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 7, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: cust.avail, EntryType: "CREDIT", Amount: 8, Currency: "RUB"},
			},
		})
		requireCode(t, err, "PC400")
		var journalsAfter, availAfter int64
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&journalsAfter); err != nil {
			t.Fatal(err)
		}
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, cust.accountID).Scan(&availAfter); err != nil {
			t.Fatal(err)
		}
		if journalsAfter != journalsBefore || availAfter != availBefore {
			t.Fatalf("rollback leaked journals %d→%d avail %d→%d", journalsBefore, journalsAfter, availBefore, availAfter)
		}
	})

	t.Run("app cannot DML ledger", func(t *testing.T) {
		mustFail(t, ctx, app, `INSERT INTO ledger.journal_transactions (
			id, operation, reference_type, reference_id, accounting_date, status)
			VALUES ($1, 'SIM_TOPUP', 'SIM_TOPUP', $1, CURRENT_DATE, 'POSTED')`, ids.next())
		mustFail(t, ctx, app, `UPDATE ledger.account_balances SET available_amount = 0 WHERE account_id = $1`, cust.accountID)
		mustFail(t, ctx, app, `INSERT INTO ledger.holds (
			id, account_id, currency, amount, status, reference_type, reference_id, expires_at)
			VALUES ($1, $2, 'RUB', 1, 'ACTIVE', 'PAYMENT', $1, now())`, ids.next(), cust.accountID)
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT * FROM ledger.account_balances WHERE account_id = $1 FOR UPDATE`, cust.accountID); err == nil {
			t.Fatal("app FOR UPDATE on account_balances")
		}
	})

	t.Run("hold place then non-active capture PC409", func(t *testing.T) {
		funded := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-HOLD")
		_, err := postJournal(ctx, app, []string{funded.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "SIM_TOPUP",
			referenceType: "SIM_TOPUP",
			referenceID:   ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 500, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "CREDIT", Amount: 500, Currency: "RUB"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		holdID := ids.next()
		payRef := ids.next()
		_, err = postJournal(ctx, app, []string{funded.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "HOLD_PLACE",
			referenceType: "HOLD",
			referenceID:   holdID,
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "DEBIT", Amount: 100, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: funded.hold, EntryType: "CREDIT", Amount: 100, Currency: "RUB"},
			},
			hold: map[string]any{
				"action":         "PLACE",
				"hold_id":        holdID,
				"account_id":     funded.accountID,
				"amount":         100,
				"currency":       "RUB",
				"expires_at":     time.Now().UTC().Add(time.Hour),
				"reference_type": "PAYMENT",
				"reference_id":   payRef,
			},
		})
		if err != nil {
			t.Fatalf("HOLD_PLACE: %v", err)
		}
		if got := readHold(t, ctx, app, holdID); got.Status != "ACTIVE" || got.Amount != 100 || got.AccountID != funded.accountID {
			t.Fatalf("placed hold = %+v", got)
		}
		var avail, reserved int64
		if err := app.QueryRow(ctx, `SELECT available_amount, reserved_amount FROM ledger.account_balances WHERE account_id = $1`, funded.accountID).Scan(&avail, &reserved); err != nil {
			t.Fatal(err)
		}
		if avail != 400 || reserved != 100 {
			t.Fatalf("after hold avail=%d reserved=%d", avail, reserved)
		}

		_, err = postJournal(ctx, app, []string{funded.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "HOLD_RELEASE",
			referenceType: "HOLD",
			referenceID:   ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: funded.hold, EntryType: "DEBIT", Amount: 100, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "CREDIT", Amount: 100, Currency: "RUB"},
			},
			hold: map[string]any{
				"action":     "RELEASE",
				"hold_id":    holdID,
				"account_id": funded.accountID,
				"amount":     100,
				"currency":   "RUB",
			},
		})
		if err != nil {
			t.Fatalf("HOLD_RELEASE: %v", err)
		}

		hold2 := ids.next()
		_, err = postJournal(ctx, app, []string{funded.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "HOLD_PLACE",
			referenceType: "HOLD",
			referenceID:   hold2,
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "DEBIT", Amount: 100, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: funded.hold, EntryType: "CREDIT", Amount: 100, Currency: "RUB"},
			},
			hold: map[string]any{
				"action":         "PLACE",
				"hold_id":        hold2,
				"account_id":     funded.accountID,
				"amount":         100,
				"currency":       "RUB",
				"expires_at":     time.Now().UTC().Add(time.Hour),
				"reference_type": "PAYMENT",
				"reference_id":   ids.next(),
			},
		})
		if err != nil {
			t.Fatalf("second HOLD_PLACE: %v", err)
		}
		if _, err := owner.Exec(ctx, `UPDATE ledger.holds SET status = 'CAPTURED', finalized_at = now() WHERE id = $1`, hold2); err != nil {
			t.Fatal(err)
		}
		merch := seedMerchant(t, ctx, owner, app, ids, "PC-PJ-M1")
		_, err = postJournal(ctx, app, []string{funded.accountID, merch.accountID}, postArgs{
			journalID:     ids.next(),
			operation:     "HOLD_CAPTURE",
			referenceType: "HOLD",
			referenceID:   ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: funded.hold, EntryType: "DEBIT", Amount: 100, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: merch.avail, EntryType: "CREDIT", Amount: 100, Currency: "RUB"},
			},
			hold: map[string]any{
				"action":     "CAPTURE",
				"hold_id":    hold2,
				"account_id": funded.accountID,
				"amount":     100,
				"currency":   "RUB",
			},
		})
		requireCode(t, err, "PC409")
	})

	t.Run("pc460 nowait while status change holds the row", func(t *testing.T) {
		target := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-460")
		blocker, err := pgx.Connect(ctx, c.app)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Close(ctx)
		btx, err := blocker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = btx.Rollback(ctx) }()
		if _, err := btx.Exec(ctx, `SELECT id FROM accounts.accounts WHERE id = $1 FOR NO KEY UPDATE`, target.accountID); err != nil {
			t.Fatal(err)
		}

		started := time.Now()
		_, err = postJournal(ctx, app, nil, postArgs{
			journalID:     ids.next(),
			operation:     "SIM_TOPUP",
			referenceType: "SIM_TOPUP",
			referenceID:   ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 3, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: target.avail, EntryType: "CREDIT", Amount: 3, Currency: "RUB"},
			},
		})
		elapsed := time.Since(started)
		requireCode(t, err, "PC460")
		if elapsed > 2*time.Second {
			t.Fatalf("PC460 waited %s, want NOWAIT", elapsed)
		}
	})

	t.Run("concurrent topups and drain", func(t *testing.T) {
		acc := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-RACE")
		peer := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-PEER")

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				conn, err := pgx.Connect(ctx, c.app)
				if err != nil {
					errs[i] = err
					return
				}
				defer conn.Close(ctx)
				<-start
				_, errs[i] = postJournal(ctx, conn, []string{acc.accountID}, postArgs{
					journalID:     ids.next(),
					operation:     "SIM_TOPUP",
					referenceType: "SIM_TOPUP",
					referenceID:   ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 8000, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "CREDIT", Amount: 8000, Currency: "RUB"},
					},
				})
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent topup %d: %v", i, err)
			}
		}
		var avail int64
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, acc.accountID).Scan(&avail); err != nil {
			t.Fatal(err)
		}
		if avail != 16000 {
			t.Fatalf("after concurrent topups avail=%d", avail)
		}

		start = make(chan struct{})
		errs = make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				conn, err := pgx.Connect(ctx, c.app)
				if err != nil {
					errs[i] = err
					return
				}
				defer conn.Close(ctx)
				<-start
				_, errs[i] = postJournal(ctx, conn, []string{acc.accountID, peer.accountID}, postArgs{
					journalID:     ids.next(),
					operation:     "TRANSFER",
					referenceType: "TRANSFER",
					referenceID:   ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "DEBIT", Amount: 10000, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: peer.avail, EntryType: "CREDIT", Amount: 10000, Currency: "RUB"},
					},
				})
			}(i)
		}
		close(start)
		wg.Wait()
		ok, pc402 := 0, 0
		for _, err := range errs {
			if err == nil {
				ok++
				continue
			}
			if pgCode(err) == "PC402" {
				pc402++
				continue
			}
			t.Fatalf("drain err = %v", err)
		}
		if ok != 1 || pc402 != 1 {
			t.Fatalf("drain results ok=%d pc402=%d", ok, pc402)
		}
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, acc.accountID).Scan(&avail); err != nil {
			t.Fatal(err)
		}
		if avail != 6000 {
			t.Fatalf("after drain avail=%d want 6000", avail)
		}
	})

	t.Run("strict operation forms PC400", func(t *testing.T) {
		funded := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-FORM")
		topup(t, ctx, app, ids, funded, 5000)
		peer := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-FORM-P")
		merch := seedMerchant(t, ctx, owner, app, ids, "PC-PJ-FORM-M")
		topup(t, ctx, app, ids, peer, 1000)
		holdID, payRef, exp := ids.next(), ids.next(), time.Date(2026, 12, 1, 15, 0, 0, 0, time.UTC)
		placeHold(t, ctx, app, ids, funded, holdID, payRef, 400, exp)
		placeHold(t, ctx, app, ids, peer, ids.next(), ids.next(), 400, exp)
		before := readState(t, ctx, app, funded.accountID)
		holdBefore := readHold(t, ctx, app, holdID)

		cases := []struct {
			name string
			lock []string
			args postArgs
			want string
		}{
			{
				name: "SIM_TOPUP uses 4100",
				lock: []string{funded.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: sys4100, EntryType: "DEBIT", Amount: 10, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "CREDIT", Amount: 10, Currency: "RUB"},
					},
				},
				want: "PC400",
			},
			{
				name: "SIM_TOPUP with hold_action",
				lock: []string{funded.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 10, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "CREDIT", Amount: 10, Currency: "RUB"},
					},
					hold: map[string]any{"action": "PLACE", "hold_id": ids.next(), "account_id": funded.accountID, "amount": 10, "currency": "RUB"},
				},
				want: "PC400",
			},
			{
				name: "TRANSFER uses 4200",
				lock: []string{funded.accountID, peer.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "TRANSFER", referenceType: "TRANSFER", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "DEBIT", Amount: 50, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: peer.avail, EntryType: "CREDIT", Amount: 40, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: sys4200, EntryType: "CREDIT", Amount: 10, Currency: "RUB"},
					},
				},
				want: "PC400",
			},
			{
				name: "TRANSFER uses 1100",
				lock: []string{funded.accountID, peer.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "TRANSFER", referenceType: "TRANSFER", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "DEBIT", Amount: 50, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: peer.avail, EntryType: "CREDIT", Amount: 40, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "CREDIT", Amount: 10, Currency: "RUB"},
					},
				},
				want: "PC400",
			},
			{
				name: "PAYOUT uses 4100",
				lock: []string{merch.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "PAYOUT", referenceType: "PAYOUT", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: merch.avail, EntryType: "DEBIT", Amount: 10, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: sys4100, EntryType: "CREDIT", Amount: 10, Currency: "RUB"},
					},
				},
				want: "PC400",
			},
			{
				name: "REFUND uses 1100",
				lock: []string{merch.accountID, funded.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "REFUND", referenceType: "REFUND", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 10, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "CREDIT", Amount: 10, Currency: "RUB"},
					},
				},
				want: "PC400",
			},
			{
				name: "HOLD_PLACE amount mismatch",
				lock: []string{funded.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "HOLD_PLACE", referenceType: "HOLD", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "DEBIT", Amount: 80, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: funded.hold, EntryType: "CREDIT", Amount: 80, Currency: "RUB"},
					},
					hold: holdPlace(ids.next(), funded.accountID, ids.next(), 100, exp),
				},
				want: "PC400",
			},
			{
				name: "HOLD_PLACE other account",
				lock: []string{funded.accountID, peer.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "HOLD_PLACE", referenceType: "HOLD", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: peer.avail, EntryType: "DEBIT", Amount: 50, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: peer.hold, EntryType: "CREDIT", Amount: 50, Currency: "RUB"},
					},
					hold: holdPlace(ids.next(), funded.accountID, ids.next(), 50, exp),
				},
				want: "PC400",
			},
			{
				name: "HOLD_CAPTURE uses 4100",
				lock: []string{funded.accountID, merch.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "HOLD_CAPTURE", referenceType: "HOLD", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: funded.hold, EntryType: "DEBIT", Amount: 400, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: merch.avail, EntryType: "CREDIT", Amount: 390, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: sys4100, EntryType: "CREDIT", Amount: 10, Currency: "RUB"},
					},
					hold: holdCapture(holdID, funded.accountID, 400),
				},
				want: "PC400",
			},
			{
				name: "HOLD_CAPTURE wrong action",
				lock: []string{funded.accountID, merch.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "HOLD_CAPTURE", referenceType: "HOLD", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: funded.hold, EntryType: "DEBIT", Amount: 400, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: merch.avail, EntryType: "CREDIT", Amount: 400, Currency: "RUB"},
					},
					hold: map[string]any{
						"action": "PLACE", "hold_id": holdID, "account_id": funded.accountID,
						"amount": 400, "currency": "RUB", "expires_at": exp,
						"reference_type": "PAYMENT", "reference_id": payRef,
					},
				},
				want: "PC400",
			},
			{
				name: "HOLD_CAPTURE other account hold",
				lock: []string{funded.accountID, peer.accountID, merch.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "HOLD_CAPTURE", referenceType: "HOLD", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: peer.hold, EntryType: "DEBIT", Amount: 400, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: merch.avail, EntryType: "CREDIT", Amount: 400, Currency: "RUB"},
					},
					hold: holdCapture(holdID, peer.accountID, 400),
				},
				want: "PC400",
			},
			{
				name: "HOLD_RELEASE amount mismatch",
				lock: []string{funded.accountID},
				args: postArgs{
					journalID: ids.next(), operation: "HOLD_RELEASE", referenceType: "HOLD", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: funded.hold, EntryType: "DEBIT", Amount: 400, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: funded.avail, EntryType: "CREDIT", Amount: 400, Currency: "RUB"},
					},
					hold: map[string]any{"action": "RELEASE", "hold_id": holdID, "account_id": funded.accountID, "amount": 50, "currency": "RUB"},
				},
				want: "PC400",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := postJournal(ctx, app, tc.lock, tc.args)
				requireCode(t, err, tc.want)
				requireState(t, readState(t, ctx, app, funded.accountID), before)
				if got := readHold(t, ctx, app, holdID); got != holdBefore {
					t.Fatalf("hold mutated: %+v → %+v", holdBefore, got)
				}
			})
		}
	})

	t.Run("idempotency hold_action reverses adjustment", func(t *testing.T) {
		acc := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-IDEM")
		topup(t, ctx, app, ids, acc, 2000)
		ref := ids.next()
		e1, e2 := ids.next(), ids.next()
		entries := []pjEntry{
			{ID: e1, LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 300, Currency: "RUB"},
			{ID: e2, LedgerAccountID: acc.avail, EntryType: "CREDIT", Amount: 300, Currency: "RUB"},
		}
		first, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ref, entries: entries,
		})
		if err != nil {
			t.Fatal(err)
		}
		before := readState(t, ctx, app, acc.accountID)
		replay, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ref, entries: entries,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !replay[0].AlreadyPosted || replay[0].JournalID != first[0].JournalID {
			t.Fatalf("replay = %+v", replay)
		}
		requireState(t, readState(t, ctx, app, acc.accountID), before)

		_, err = postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ref, entries: entries,
			hold: map[string]any{"action": "PLACE", "hold_id": ids.next(), "account_id": acc.accountID, "amount": 300, "currency": "RUB"},
		})
		requireCode(t, err, "PC409")
		requireState(t, readState(t, ctx, app, acc.accountID), before)

		_, err = postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ref, entries: entries,
			adjustmentID: ids.next(),
		})
		requireCode(t, err, "PC409")
		requireState(t, readState(t, ctx, app, acc.accountID), before)

		holdID, payRef, exp := ids.next(), ids.next(), time.Date(2026, 12, 2, 12, 0, 0, 0, time.UTC)
		hold := holdPlace(holdID, acc.accountID, payRef, 200, exp)
		placeEntries := []pjEntry{
			{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "DEBIT", Amount: 200, Currency: "RUB"},
			{ID: ids.next(), LedgerAccountID: acc.hold, EntryType: "CREDIT", Amount: 200, Currency: "RUB"},
		}
		holdRef := ids.next()
		placed, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "HOLD_PLACE", referenceType: "HOLD", referenceID: holdRef,
			entries: placeEntries, hold: hold,
		})
		if err != nil {
			t.Fatal(err)
		}
		afterPlace := readState(t, ctx, app, acc.accountID)
		holdReplay, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "HOLD_PLACE", referenceType: "HOLD", referenceID: holdRef,
			entries: placeEntries, hold: hold,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !holdReplay[0].AlreadyPosted || holdReplay[0].JournalID != placed[0].JournalID {
			t.Fatalf("hold replay = %+v", holdReplay)
		}
		requireState(t, readState(t, ctx, app, acc.accountID), afterPlace)

		diffExp := holdPlace(holdID, acc.accountID, payRef, 200, exp.Add(time.Hour))
		_, err = postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "HOLD_PLACE", referenceType: "HOLD", referenceID: holdRef,
			entries: placeEntries, hold: diffExp,
		})
		requireCode(t, err, "PC409")
		requireState(t, readState(t, ctx, app, acc.accountID), afterPlace)
		if got := readHold(t, ctx, app, holdID); got.Status != "ACTIVE" || got.Amount != 200 {
			t.Fatalf("hold after idempotency conflict = %+v", got)
		}

		origJID := first[0].JournalID
		revEntries := []pjEntry{
			{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "DEBIT", Amount: 300, Currency: "RUB"},
			{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "CREDIT", Amount: 300, Currency: "RUB"},
		}
		rev, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "REVERSAL", referenceType: "SIM_TOPUP", referenceID: ref,
			reversesJournalID: origJID, entries: revEntries,
		})
		if err != nil {
			t.Fatal(err)
		}
		afterRev := readState(t, ctx, app, acc.accountID)
		revReplay, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "REVERSAL", referenceType: "SIM_TOPUP", referenceID: ref,
			reversesJournalID: origJID, entries: revEntries,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !revReplay[0].AlreadyPosted || revReplay[0].JournalID != rev[0].JournalID {
			t.Fatalf("reversal replay = %+v", revReplay)
		}
		requireState(t, readState(t, ctx, app, acc.accountID), afterRev)

		otherTopup, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 10, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "CREDIT", Amount: 10, Currency: "RUB"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		afterOther := readState(t, ctx, app, acc.accountID)
		_, err = postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "REVERSAL", referenceType: "SIM_TOPUP", referenceID: ref,
			reversesJournalID: otherTopup[0].JournalID, entries: revEntries,
		})
		requireCode(t, err, "PC409")
		requireState(t, readState(t, ctx, app, acc.accountID), afterOther)

		// adjustment_id is not persisted (00003). Replay of this REVERSAL with a
		// different adjustment_id still matches stored identity → already_posted.
		revReplayAdj, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "REVERSAL", referenceType: "SIM_TOPUP", referenceID: ref,
			reversesJournalID: origJID, entries: revEntries, adjustmentID: ids.next(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !revReplayAdj[0].AlreadyPosted || revReplayAdj[0].JournalID != rev[0].JournalID {
			t.Fatalf("reversal replay with other adjustment_id = %+v", revReplayAdj)
		}
		requireState(t, readState(t, ctx, app, acc.accountID), afterOther)
	})

	t.Run("hold capture release and other-account", func(t *testing.T) {
		custA := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-HA")
		custB := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-HB")
		merch := seedMerchant(t, ctx, owner, app, ids, "PC-PJ-HM")
		topup(t, ctx, app, ids, custA, 1000)
		topup(t, ctx, app, ids, custB, 1000)
		exp := time.Date(2026, 12, 3, 8, 0, 0, 0, time.UTC)
		holdA, payA := ids.next(), ids.next()
		holdB, payB := ids.next(), ids.next()
		placeHold(t, ctx, app, ids, custA, holdA, payA, 250, exp)
		placeHold(t, ctx, app, ids, custB, holdB, payB, 250, exp)

		beforeA := readState(t, ctx, app, custA.accountID)
		beforeB := readState(t, ctx, app, custB.accountID)
		_, err := postJournal(ctx, app, []string{custA.accountID, custB.accountID, merch.accountID}, postArgs{
			journalID: ids.next(), operation: "HOLD_CAPTURE", referenceType: "HOLD", referenceID: ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: custB.hold, EntryType: "DEBIT", Amount: 250, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: merch.avail, EntryType: "CREDIT", Amount: 250, Currency: "RUB"},
			},
			hold: holdCapture(holdA, custB.accountID, 250),
		})
		requireCode(t, err, "PC400")
		requireState(t, readState(t, ctx, app, custA.accountID), beforeA)
		requireState(t, readState(t, ctx, app, custB.accountID), beforeB)
		if got := readHold(t, ctx, app, holdA); got.Status != "ACTIVE" {
			t.Fatalf("hold A status = %s", got.Status)
		}

		capRows, err := postJournal(ctx, app, []string{custA.accountID, merch.accountID}, postArgs{
			journalID: ids.next(), operation: "HOLD_CAPTURE", referenceType: "HOLD", referenceID: ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: custA.hold, EntryType: "DEBIT", Amount: 250, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: merch.avail, EntryType: "CREDIT", Amount: 240, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: sys4200, EntryType: "CREDIT", Amount: 10, Currency: "RUB"},
			},
			hold: holdCapture(holdA, custA.accountID, 250),
		})
		if err != nil {
			t.Fatal(err)
		}
		if capRows[0].AlreadyPosted {
			t.Fatal("capture already_posted")
		}
		var avail, reserved int64
		if err := app.QueryRow(ctx, `SELECT available_amount, reserved_amount FROM ledger.account_balances WHERE account_id = $1`, custA.accountID).Scan(&avail, &reserved); err != nil {
			t.Fatal(err)
		}
		if avail != 750 || reserved != 0 {
			t.Fatalf("after capture A avail=%d reserved=%d", avail, reserved)
		}
		if got := readHold(t, ctx, app, holdA); got.Status != "CAPTURED" {
			t.Fatalf("hold A = %+v", got)
		}
		var merchAvail int64
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, merch.accountID).Scan(&merchAvail); err != nil {
			t.Fatal(err)
		}
		if merchAvail != 240 {
			t.Fatalf("merchant after capture = %d", merchAvail)
		}

		afterCap := readState(t, ctx, app, custA.accountID)
		_, err = postJournal(ctx, app, []string{custA.accountID}, postArgs{
			journalID: ids.next(), operation: "HOLD_RELEASE", referenceType: "HOLD", referenceID: ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: custA.hold, EntryType: "DEBIT", Amount: 250, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: custA.avail, EntryType: "CREDIT", Amount: 250, Currency: "RUB"},
			},
			hold: map[string]any{"action": "RELEASE", "hold_id": holdA, "account_id": custA.accountID, "amount": 250, "currency": "RUB"},
		})
		requireCode(t, err, "PC402")
		requireState(t, readState(t, ctx, app, custA.accountID), afterCap)
		if got := readHold(t, ctx, app, holdA); got.Status != "CAPTURED" {
			t.Fatalf("captured hold mutated = %+v", got)
		}

		rel, err := postJournal(ctx, app, []string{custB.accountID}, postArgs{
			journalID: ids.next(), operation: "HOLD_RELEASE", referenceType: "HOLD", referenceID: ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: custB.hold, EntryType: "DEBIT", Amount: 250, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: custB.avail, EntryType: "CREDIT", Amount: 250, Currency: "RUB"},
			},
			hold: map[string]any{"action": "RELEASE", "hold_id": holdB, "account_id": custB.accountID, "amount": 250, "currency": "RUB"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if rel[0].AlreadyPosted {
			t.Fatal("release already_posted")
		}
		if err := app.QueryRow(ctx, `SELECT available_amount, reserved_amount FROM ledger.account_balances WHERE account_id = $1`, custB.accountID).Scan(&avail, &reserved); err != nil {
			t.Fatal(err)
		}
		if avail != 1000 || reserved != 0 {
			t.Fatalf("after release B avail=%d reserved=%d", avail, reserved)
		}
		if got := readHold(t, ctx, app, holdB); got.Status != "RELEASED" {
			t.Fatalf("hold B = %+v", got)
		}

		dupBefore := readState(t, ctx, app, custA.accountID)
		_, err = postJournal(ctx, app, []string{custA.accountID}, postArgs{
			journalID: ids.next(), operation: "HOLD_PLACE", referenceType: "HOLD", referenceID: ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: custA.avail, EntryType: "DEBIT", Amount: 100, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: custA.hold, EntryType: "CREDIT", Amount: 100, Currency: "RUB"},
			},
			hold: holdPlace(holdA, custA.accountID, ids.next(), 100, exp),
		})
		requireCode(t, err, "PC409")
		requireState(t, readState(t, ctx, app, custA.accountID), dupBefore)
		if got := readHold(t, ctx, app, holdA); got.Status != "CAPTURED" || got.Amount != 250 {
			t.Fatalf("duplicate place mutated hold A = %+v", got)
		}
	})

	t.Run("concurrent holds capture vs release mixed ops", func(t *testing.T) {
		acc := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-PAR")
		peer := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-PAR-P")
		merch := seedMerchant(t, ctx, owner, app, ids, "PC-PJ-PAR-M")
		topup(t, ctx, app, ids, acc, 20000)
		exp := time.Date(2026, 12, 4, 9, 0, 0, 0, time.UTC)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		holds := []string{ids.next(), ids.next()}
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				conn, err := pgx.Connect(ctx, c.app)
				if err != nil {
					errs[i] = err
					return
				}
				defer conn.Close(ctx)
				<-start
				_, errs[i] = postJournal(ctx, conn, []string{acc.accountID}, postArgs{
					journalID: ids.next(), operation: "HOLD_PLACE", referenceType: "HOLD", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "DEBIT", Amount: 3000, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: acc.hold, EntryType: "CREDIT", Amount: 3000, Currency: "RUB"},
					},
					hold: holdPlace(holds[i], acc.accountID, ids.next(), 3000, exp),
				})
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("parallel hold %d: %v", i, err)
			}
		}
		var avail, reserved int64
		if err := app.QueryRow(ctx, `SELECT available_amount, reserved_amount FROM ledger.account_balances WHERE account_id = $1`, acc.accountID).Scan(&avail, &reserved); err != nil {
			t.Fatal(err)
		}
		if avail != 14000 || reserved != 6000 {
			t.Fatalf("after parallel holds avail=%d reserved=%d", avail, reserved)
		}
		for _, id := range holds {
			if got := readHold(t, ctx, app, id); got.Status != "ACTIVE" || got.Amount != 3000 {
				t.Fatalf("parallel hold %s = %+v", id, got)
			}
		}

		holdCR := ids.next()
		placeHold(t, ctx, app, ids, acc, holdCR, ids.next(), 2000, exp)
		start = make(chan struct{})
		errs = make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				conn, err := pgx.Connect(ctx, c.app)
				if err != nil {
					errs[i] = err
					return
				}
				defer conn.Close(ctx)
				<-start
				if i == 0 {
					_, errs[i] = postJournal(ctx, conn, []string{acc.accountID, merch.accountID}, postArgs{
						journalID: ids.next(), operation: "HOLD_CAPTURE", referenceType: "HOLD", referenceID: ids.next(),
						entries: []pjEntry{
							{ID: ids.next(), LedgerAccountID: acc.hold, EntryType: "DEBIT", Amount: 2000, Currency: "RUB"},
							{ID: ids.next(), LedgerAccountID: merch.avail, EntryType: "CREDIT", Amount: 2000, Currency: "RUB"},
						},
						hold: holdCapture(holdCR, acc.accountID, 2000),
					})
					return
				}
				_, errs[i] = postJournal(ctx, conn, []string{acc.accountID}, postArgs{
					journalID: ids.next(), operation: "HOLD_RELEASE", referenceType: "HOLD", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: acc.hold, EntryType: "DEBIT", Amount: 2000, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "CREDIT", Amount: 2000, Currency: "RUB"},
					},
					hold: map[string]any{"action": "RELEASE", "hold_id": holdCR, "account_id": acc.accountID, "amount": 2000, "currency": "RUB"},
				})
			}(i)
		}
		close(start)
		wg.Wait()
		ok, pc409 := 0, 0
		for _, err := range errs {
			if err == nil {
				ok++
				continue
			}
			if pgCode(err) == "PC409" {
				pc409++
				continue
			}
			t.Fatalf("capture vs release err = %v", err)
		}
		if ok != 1 || pc409 != 1 {
			t.Fatalf("capture vs release ok=%d pc409=%d errs=%v", ok, pc409, errs)
		}
		gotHold := readHold(t, ctx, app, holdCR)
		if gotHold.Status != "CAPTURED" && gotHold.Status != "RELEASED" {
			t.Fatalf("hold after race = %+v", gotHold)
		}
		if err := app.QueryRow(ctx, `SELECT available_amount, reserved_amount FROM ledger.account_balances WHERE account_id = $1`, acc.accountID).Scan(&avail, &reserved); err != nil {
			t.Fatal(err)
		}
		if reserved != 6000 {
			t.Fatalf("reserved after capture/release race = %d want 6000 (two leftover holds)", reserved)
		}
		var merchAvail int64
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, merch.accountID).Scan(&merchAvail); err != nil {
			t.Fatal(err)
		}
		switch gotHold.Status {
		case "CAPTURED":
			if merchAvail != 2000 {
				t.Fatalf("merchant after capture race = %d", merchAvail)
			}
		case "RELEASED":
			if merchAvail != 0 {
				t.Fatalf("merchant after release race = %d", merchAvail)
			}
		}

		start = make(chan struct{})
		errs = make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				conn, err := pgx.Connect(ctx, c.app)
				if err != nil {
					errs[i] = err
					return
				}
				defer conn.Close(ctx)
				<-start
				if i == 0 {
					_, errs[i] = postJournal(ctx, conn, []string{acc.accountID, peer.accountID}, postArgs{
						journalID: ids.next(), operation: "TRANSFER", referenceType: "TRANSFER", referenceID: ids.next(),
						entries: []pjEntry{
							{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "DEBIT", Amount: 1000, Currency: "RUB"},
							{ID: ids.next(), LedgerAccountID: peer.avail, EntryType: "CREDIT", Amount: 1000, Currency: "RUB"},
						},
					})
					return
				}
				_, errs[i] = postJournal(ctx, conn, []string{acc.accountID}, postArgs{
					journalID: ids.next(), operation: "HOLD_PLACE", referenceType: "HOLD", referenceID: ids.next(),
					entries: []pjEntry{
						{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "DEBIT", Amount: 1000, Currency: "RUB"},
						{ID: ids.next(), LedgerAccountID: acc.hold, EntryType: "CREDIT", Amount: 1000, Currency: "RUB"},
					},
					hold: holdPlace(ids.next(), acc.accountID, ids.next(), 1000, exp),
				})
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("mixed op %d: %v", i, err)
			}
		}
		if err := app.QueryRow(ctx, `SELECT available_amount, reserved_amount FROM ledger.account_balances WHERE account_id = $1`, acc.accountID).Scan(&avail, &reserved); err != nil {
			t.Fatal(err)
		}
		if reserved != 7000 {
			t.Fatalf("after mixed ops reserved=%d want 7000", reserved)
		}
		var peerAvail int64
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, peer.accountID).Scan(&peerAvail); err != nil {
			t.Fatal(err)
		}
		if peerAvail != 1000 {
			t.Fatalf("peer after mixed ops = %d", peerAvail)
		}
	})

	t.Run("idempotency date initiator description", func(t *testing.T) {
		acc := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-META")
		ref := ids.next()
		initiator := ids.next()
		day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
		e1, e2 := ids.next(), ids.next()
		entries := []pjEntry{
			{ID: e1, LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 80, Currency: "RUB"},
			{ID: e2, LedgerAccountID: acc.avail, EntryType: "CREDIT", Amount: 80, Currency: "RUB"},
		}
		first, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ref,
			entries: entries, description: "orig-desc", initiatedBy: initiator, accountingDate: day,
		})
		if err != nil {
			t.Fatal(err)
		}
		orig := readJournal(t, ctx, app, first[0].JournalID)
		if orig.Description != "orig-desc" || orig.InitiatedBy != initiator {
			t.Fatalf("stored meta = %+v", orig)
		}
		before := readState(t, ctx, app, acc.accountID)

		replayDesc, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ref,
			entries: entries, description: "other-desc", initiatedBy: initiator, accountingDate: day,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !replayDesc[0].AlreadyPosted || replayDesc[0].JournalID != first[0].JournalID {
			t.Fatalf("desc replay = %+v", replayDesc)
		}
		requireState(t, readState(t, ctx, app, acc.accountID), before)
		requireJournal(t, readJournal(t, ctx, app, first[0].JournalID), orig)

		replayInit, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ref,
			entries: entries, description: "orig-desc", initiatedBy: ids.next(), accountingDate: day,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !replayInit[0].AlreadyPosted || replayInit[0].JournalID != first[0].JournalID {
			t.Fatalf("initiator replay = %+v", replayInit)
		}
		requireState(t, readState(t, ctx, app, acc.accountID), before)
		requireJournal(t, readJournal(t, ctx, app, first[0].JournalID), orig)

		_, err = postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ref,
			entries: entries, description: "orig-desc", initiatedBy: initiator,
			accountingDate: day.Add(24 * time.Hour),
		})
		requireCode(t, err, "PC409")
		requireState(t, readState(t, ctx, app, acc.accountID), before)
		requireJournal(t, readJournal(t, ctx, app, first[0].JournalID), orig)
	})

	t.Run("HOLD_CAPTURE fee zero two entries", func(t *testing.T) {
		cust := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-F0")
		merch := seedMerchant(t, ctx, owner, app, ids, "PC-PJ-F0M")
		topup(t, ctx, app, ids, cust, 500)
		holdID := ids.next()
		placeHold(t, ctx, app, ids, cust, holdID, ids.next(), 200, time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC))
		rows, err := postJournal(ctx, app, []string{cust.accountID, merch.accountID}, postArgs{
			journalID: ids.next(), operation: "HOLD_CAPTURE", referenceType: "HOLD", referenceID: ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: cust.hold, EntryType: "DEBIT", Amount: 200, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: merch.avail, EntryType: "CREDIT", Amount: 200, Currency: "RUB"},
			},
			hold: holdCapture(holdID, cust.accountID, 200),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 || rows[0].AlreadyPosted {
			t.Fatalf("fee=0 capture = %+v", rows)
		}
		var avail, reserved, merchAvail int64
		if err := app.QueryRow(ctx, `SELECT available_amount, reserved_amount FROM ledger.account_balances WHERE account_id = $1`, cust.accountID).Scan(&avail, &reserved); err != nil {
			t.Fatal(err)
		}
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, merch.accountID).Scan(&merchAvail); err != nil {
			t.Fatal(err)
		}
		if avail != 300 || reserved != 0 || merchAvail != 200 {
			t.Fatalf("fee=0 capture avail=%d reserved=%d merch=%d", avail, reserved, merchAvail)
		}
		if got := readHold(t, ctx, app, holdID); got.Status != "CAPTURED" {
			t.Fatalf("hold after fee=0 capture = %+v", got)
		}
		var n4200 int64
		if err := app.QueryRow(ctx, `
			SELECT count(*) FROM ledger.ledger_entries e
			 JOIN ledger.journal_transactions j ON j.id = e.journal_transaction_id
			 WHERE j.id = $1 AND e.ledger_account_id = $2`, rows[0].JournalID, sys4200).Scan(&n4200); err != nil {
			t.Fatal(err)
		}
		if n4200 != 0 {
			t.Fatalf("fee=0 capture wrote 4200 entries: %d", n4200)
		}
	})

	t.Run("manual_adjustment approval", func(t *testing.T) {
		acc := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-ADJ")
		ref := ids.next()
		topupRows, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ref,
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: 400, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "CREDIT", Amount: 400, Currency: "RUB"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := app.Exec(ctx, `UPDATE accounts.accounts SET status = 'BLOCKED', version = version + 1 WHERE id = $1`, acc.accountID); err != nil {
			t.Fatal(err)
		}
		before := readState(t, ctx, app, acc.accountID)
		revEntries := []pjEntry{
			{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "DEBIT", Amount: 400, Currency: "RUB"},
			{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "CREDIT", Amount: 400, Currency: "RUB"},
		}
		_, err = postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "REVERSAL", referenceType: "SIM_TOPUP", referenceID: ref,
			reversesJournalID: topupRows[0].JournalID, entries: revEntries,
		})
		requireCode(t, err, "PC403")
		requireState(t, readState(t, ctx, app, acc.accountID), before)

		draft := insertAdjustment(t, ctx, app, ids, "DRAFT")
		_, err = postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "REVERSAL", referenceType: "SIM_TOPUP", referenceID: ref,
			reversesJournalID: topupRows[0].JournalID, entries: revEntries, adjustmentID: draft,
		})
		requireCode(t, err, "PC403")
		requireState(t, readState(t, ctx, app, acc.accountID), before)

		approved := insertAdjustment(t, ctx, app, ids, "APPROVED")
		rev, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "REVERSAL", referenceType: "SIM_TOPUP", referenceID: ref,
			reversesJournalID: topupRows[0].JournalID, entries: revEntries, adjustmentID: approved,
		})
		if err != nil {
			t.Fatal(err)
		}
		if rev[0].AlreadyPosted {
			t.Fatal("approved reversal already_posted")
		}
		var avail int64
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, acc.accountID).Scan(&avail); err != nil {
			t.Fatal(err)
		}
		if avail != 0 {
			t.Fatalf("after approved reversal avail=%d", avail)
		}

		other := insertAdjustment(t, ctx, app, ids, "APPROVED")
		afterRev := readState(t, ctx, app, acc.accountID)
		replay, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
			journalID: ids.next(), operation: "REVERSAL", referenceType: "SIM_TOPUP", referenceID: ref,
			reversesJournalID: topupRows[0].JournalID, entries: revEntries, adjustmentID: other,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !replay[0].AlreadyPosted || replay[0].JournalID != rev[0].JournalID {
			t.Fatalf("blocked reversal replay other adjustment_id = %+v", replay)
		}
		requireState(t, readState(t, ctx, app, acc.accountID), afterRev)

		blocked := seedCustomer(t, ctx, owner, app, ids, "PC-PJ-ADJ2")
		if _, err := app.Exec(ctx, `UPDATE accounts.accounts SET status = 'BLOCKED', version = version + 1 WHERE id = $1`, blocked.accountID); err != nil {
			t.Fatal(err)
		}
		blkBefore := readState(t, ctx, app, blocked.accountID)
		_, err = postJournal(ctx, app, []string{blocked.accountID}, postArgs{
			journalID: ids.next(), operation: "ADJUSTMENT", referenceType: "ADJUSTMENT", referenceID: ids.next(),
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys5900, EntryType: "DEBIT", Amount: 25, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: blocked.avail, EntryType: "CREDIT", Amount: 25, Currency: "RUB"},
			},
		})
		requireCode(t, err, "PC403")
		requireState(t, readState(t, ctx, app, blocked.accountID), blkBefore)

		claim := insertAdjustment(t, ctx, app, ids, "APPROVED")
		adj, err := postJournal(ctx, app, []string{blocked.accountID}, postArgs{
			journalID: ids.next(), operation: "ADJUSTMENT", referenceType: "ADJUSTMENT", referenceID: claim,
			entries: []pjEntry{
				{ID: ids.next(), LedgerAccountID: sys5900, EntryType: "DEBIT", Amount: 25, Currency: "RUB"},
				{ID: ids.next(), LedgerAccountID: blocked.avail, EntryType: "CREDIT", Amount: 25, Currency: "RUB"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if adj[0].AlreadyPosted {
			t.Fatal("approved adjustment already_posted")
		}
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, blocked.accountID).Scan(&avail); err != nil {
			t.Fatal(err)
		}
		if avail != 25 {
			t.Fatalf("after approved adjustment avail=%d", avail)
		}
	})
}

type seededAccount struct {
	accountID, userID, avail, hold string
}

func seedCustomer(t *testing.T, ctx context.Context, owner, app *pgx.Conn, ids *idgen, number string) seededAccount {
	t.Helper()
	out := seededAccount{userID: ids.next(), accountID: ids.next()}
	if _, err := owner.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1)`, out.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `
		INSERT INTO accounts.accounts (id, account_number, user_id, account_purpose, currency, status)
		VALUES ($1, $2, $3, 'MAIN', 'RUB', 'ACTIVE')`, out.accountID, number, out.userID); err != nil {
		t.Fatal(err)
	}
	out.avail, out.hold = openPair(t, ctx, app, out.accountID, "CUSTOMER")
	return out
}

func seedMerchant(t *testing.T, ctx context.Context, owner, app *pgx.Conn, ids *idgen, number string) seededAccount {
	t.Helper()
	out := seededAccount{userID: ids.next(), accountID: ids.next()}
	if _, err := owner.Exec(ctx, `INSERT INTO merchants.merchants (id) VALUES ($1)`, out.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `
		INSERT INTO accounts.accounts (id, account_number, merchant_id, account_purpose, currency, status)
		VALUES ($1, $2, $3, 'MERCHANT_SETTLEMENT', 'RUB', 'ACTIVE')`, out.accountID, number, out.userID); err != nil {
		t.Fatal(err)
	}
	out.avail, out.hold = openPair(t, ctx, app, out.accountID, "MERCHANT")
	return out
}

func openPair(t *testing.T, ctx context.Context, app *pgx.Conn, accountID, ownerType string) (avail, hold string) {
	t.Helper()
	rows, err := app.Query(ctx, `SELECT kind, ledger_account_id::text FROM ledger.open_ledger_accounts($1, $2, 'RUB')`, accountID, ownerType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "AVAILABLE":
			avail = id
		case "HOLD":
			hold = id
		}
	}
	if avail == "" || hold == "" {
		t.Fatalf("open_ledger_accounts incomplete avail=%s hold=%s", avail, hold)
	}
	return avail, hold
}

type postArgs struct {
	journalID         string
	operation         string
	referenceType     string
	referenceID       string
	entries           []pjEntry
	hold              map[string]any
	reversesJournalID string
	adjustmentID      string
	description       string
	initiatedBy       string
	accountingDate    time.Time
}

func postJournal(ctx context.Context, conn *pgx.Conn, lockIDs []string, a postArgs) ([]pjRow, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
		return nil, err
	}
	if len(lockIDs) > 0 {
		if err := shareAccounts(ctx, tx, lockIDs...); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `SELECT * FROM ledger.lock_account_balances($1::uuid[])`, lockIDs); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(a.entries)
	if err != nil {
		return nil, err
	}
	if a.entries == nil {
		raw = []byte("[]")
	}
	var hold any
	if a.hold != nil {
		hb, err := json.Marshal(a.hold)
		if err != nil {
			return nil, err
		}
		hold = string(hb)
	}
	var reverses any
	if a.reversesJournalID != "" {
		reverses = a.reversesJournalID
	}
	var adjustment any
	if a.adjustmentID != "" {
		adjustment = a.adjustmentID
	}
	desc := a.description
	if desc == "" {
		desc = "test"
	}
	var initiated any
	if a.initiatedBy != "" {
		initiated = a.initiatedBy
	}
	acctDate := a.accountingDate
	if acctDate.IsZero() {
		acctDate = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	}
	rows, err := tx.Query(ctx, `
		SELECT posted_journal_id::text, already_posted, posted_entry_id::text, posted_ledger_account_id::text,
		       posted_entry_type, posted_amount, posted_currency::text, posted_account_version, posted_balance_after
		  FROM ledger.post_journal(
			$1::uuid, $2, $3, $4::uuid, $5::smallint, $6::uuid, $7::uuid,
			$8, $9::uuid, $10::date, $11::jsonb, $12::jsonb)`,
		a.journalID, a.operation, a.referenceType, a.referenceID, int16(1),
		reverses, adjustment, desc, initiated, acctDate,
		string(raw), hold,
	)
	if err != nil {
		return nil, err
	}
	var out []pjRow
	for rows.Next() {
		var row pjRow
		if err := rows.Scan(&row.JournalID, &row.AlreadyPosted, &row.EntryID, &row.LedgerAccountID,
			&row.EntryType, &row.Amount, &row.Currency, &row.AccountVersion, &row.BalanceAfter); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func shareAccounts(ctx context.Context, tx pgx.Tx, ids ...string) error {
	_, err := tx.Exec(ctx, `SELECT id FROM accounts.accounts WHERE id = ANY($1) ORDER BY id FOR SHARE`, ids)
	return err
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func pgMsg(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Message
	}
	return ""
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want SQLSTATE %s, got nil", code)
	}
	got := pgCode(err)
	if got != code {
		t.Fatalf("want SQLSTATE %s, got %s (%v)", code, got, err)
	}
}

type ledgerState struct {
	Journals, Entries, Holds int64
	Avail, Reserved          int64
}

type holdRow struct {
	Status    string
	Amount    int64
	AccountID string
	Currency  string
}

func readState(t *testing.T, ctx context.Context, conn *pgx.Conn, accountID string) ledgerState {
	t.Helper()
	var s ledgerState
	if err := conn.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM ledger.journal_transactions),
		       (SELECT count(*) FROM ledger.ledger_entries),
		       (SELECT count(*) FROM ledger.holds),
		       (SELECT available_amount FROM ledger.account_balances WHERE account_id = $1),
		       (SELECT reserved_amount FROM ledger.account_balances WHERE account_id = $1)`,
		accountID).Scan(&s.Journals, &s.Entries, &s.Holds, &s.Avail, &s.Reserved); err != nil {
		t.Fatal(err)
	}
	return s
}

func requireState(t *testing.T, got, want ledgerState) {
	t.Helper()
	if got != want {
		t.Fatalf("ledger state = %+v want %+v", got, want)
	}
}

type journalMeta struct {
	Description    string
	InitiatedBy    string
	AccountingDate time.Time
}

func readJournal(t *testing.T, ctx context.Context, conn *pgx.Conn, journalID string) journalMeta {
	t.Helper()
	var m journalMeta
	var initiated *string
	if err := conn.QueryRow(ctx, `
		SELECT coalesce(description, ''), initiated_by::text, accounting_date
		  FROM ledger.journal_transactions WHERE id = $1`, journalID).Scan(
		&m.Description, &initiated, &m.AccountingDate); err != nil {
		t.Fatal(err)
	}
	if initiated != nil {
		m.InitiatedBy = *initiated
	}
	return m
}

func requireJournal(t *testing.T, got, want journalMeta) {
	t.Helper()
	if got.Description != want.Description || got.InitiatedBy != want.InitiatedBy {
		t.Fatalf("journal meta = %+v want %+v", got, want)
	}
	if !got.AccountingDate.Equal(want.AccountingDate) {
		t.Fatalf("accounting_date = %s want %s", got.AccountingDate.Format("2006-01-02"), want.AccountingDate.Format("2006-01-02"))
	}
}

func insertAdjustment(t *testing.T, ctx context.Context, app *pgx.Conn, ids *idgen, status string) string {
	t.Helper()
	id := ids.next()
	created := ids.next()
	var approved any
	if status == "APPROVED" {
		approved = ids.next()
	}
	if _, err := app.Exec(ctx, `
		INSERT INTO ledger.manual_adjustments (
			id, status, proposed_entries, reason, incident_ref, created_by, approved_by, decided_at)
		VALUES ($1, $2, '[]'::jsonb, 'test-reason', 'INC-TEST', $3, $4,
		        CASE WHEN $2 = 'APPROVED' THEN now() ELSE NULL END)`,
		id, status, created, approved); err != nil {
		t.Fatalf("insertAdjustment: %v", err)
	}
	return id
}

func readHold(t *testing.T, ctx context.Context, conn *pgx.Conn, holdID string) holdRow {
	t.Helper()
	var h holdRow
	if err := conn.QueryRow(ctx, `
		SELECT status, amount, account_id::text, currency::text FROM ledger.holds WHERE id = $1`,
		holdID).Scan(&h.Status, &h.Amount, &h.AccountID, &h.Currency); err != nil {
		t.Fatal(err)
	}
	return h
}

func holdPlace(holdID, accountID, payRef string, amount int64, exp time.Time) map[string]any {
	return map[string]any{
		"action":         "PLACE",
		"hold_id":        holdID,
		"account_id":     accountID,
		"amount":         amount,
		"currency":       "RUB",
		"expires_at":     exp,
		"reference_type": "PAYMENT",
		"reference_id":   payRef,
	}
}

func holdCapture(holdID, accountID string, amount int64) map[string]any {
	return map[string]any{
		"action":     "CAPTURE",
		"hold_id":    holdID,
		"account_id": accountID,
		"amount":     amount,
		"currency":   "RUB",
	}
}

func topup(t *testing.T, ctx context.Context, app *pgx.Conn, ids *idgen, acc seededAccount, amount int64) {
	t.Helper()
	_, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
		journalID: ids.next(), operation: "SIM_TOPUP", referenceType: "SIM_TOPUP", referenceID: ids.next(),
		entries: []pjEntry{
			{ID: ids.next(), LedgerAccountID: sys1100, EntryType: "DEBIT", Amount: amount, Currency: "RUB"},
			{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "CREDIT", Amount: amount, Currency: "RUB"},
		},
	})
	if err != nil {
		t.Fatalf("topup: %v", err)
	}
}

func placeHold(t *testing.T, ctx context.Context, app *pgx.Conn, ids *idgen, acc seededAccount, holdID, payRef string, amount int64, exp time.Time) {
	t.Helper()
	_, err := postJournal(ctx, app, []string{acc.accountID}, postArgs{
		journalID: ids.next(), operation: "HOLD_PLACE", referenceType: "HOLD", referenceID: ids.next(),
		entries: []pjEntry{
			{ID: ids.next(), LedgerAccountID: acc.avail, EntryType: "DEBIT", Amount: amount, Currency: "RUB"},
			{ID: ids.next(), LedgerAccountID: acc.hold, EntryType: "CREDIT", Amount: amount, Currency: "RUB"},
		},
		hold: holdPlace(holdID, acc.accountID, payRef, amount, exp),
	})
	if err != nil {
		t.Fatalf("HOLD_PLACE: %v", err)
	}
}
