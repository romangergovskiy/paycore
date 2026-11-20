package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	accountsapp "github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	accountshttp "github.com/romangergovskiy/paycore/internal/accounts/http"
	"github.com/romangergovskiy/paycore/internal/auth/application"
	authdomain "github.com/romangergovskiy/paycore/internal/auth/domain"
	authhttp "github.com/romangergovskiy/paycore/internal/auth/http"
	"github.com/romangergovskiy/paycore/internal/platform/authcrypto"
	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

// TestAccountsHTTPPostgres drives customer Accounts HTTP through BearerAuth,
// Principal, Accounts application, and PostgreSQL as paycore_app.
// Reads must not create journals/entries or mutate balances.
func TestAccountsHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := startCluster(t)
	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}

	pool, err := Open(ctx, config.Postgres{
		DSN:              c.app,
		MaxConns:         8,
		MinConns:         2,
		MaxConnLifetime:  time.Minute,
		MaxConnIdleTime:  time.Minute,
		LockTimeout:      3 * time.Second,
		StatementTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	app := mustConnect(t, ctx, c.app)
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	env := newAccountsHTTPEnv(t, ctx, pool)
	uow := NewAccountUoW(pool)

	type customer struct {
		id    uuid.UUID
		token string
	}
	registerLogin := func(t *testing.T, prefix string) customer {
		t.Helper()
		email := pgEmail(t, prefix)
		env.trackSecret(httpPGPassword)
		reg := env.do(http.MethodPost, "/api/v1/auth/register", registerBody(email, httpPGPassword), "", nextTestIP())
		if reg.Code != http.StatusCreated {
			t.Fatalf("register = %d %s", reg.Code, reg.Body)
		}
		login := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, httpPGPassword), "", nextTestIP())
		if login.Code != http.StatusOK {
			t.Fatalf("login = %d %s", login.Code, login.Body)
		}
		cred := decodeCreds(t, login.Body)
		env.trackSecret(cred.AccessToken, cred.RefreshToken)
		return customer{id: cred.User.ID, token: cred.AccessToken}
	}
	openFor := func(t *testing.T, user uuid.UUID, number string) accountsapp.Opened {
		t.Helper()
		return mustOpen(t, ctx, uow, user, number)
	}
	authz := func(c customer) string { return "Bearer " + c.token }

	t.Run("empty list", func(t *testing.T) {
		env.t = t
		a := registerLogin(t, "acc-empty")
		rec := env.do(http.MethodGet, "/api/v1/accounts", "", authz(a), nextTestIP())
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body)
		}
		got := decodeAccounts(t, rec.Body)
		if got == nil || len(got) != 0 {
			t.Fatalf("empty list = %#v", got)
		}
		if strings.Contains(rec.Body.String(), "null") {
			t.Fatalf("null list: %s", rec.Body)
		}
	})

	t.Run("list get balance statement own account", func(t *testing.T) {
		env.t = t
		a := registerLogin(t, "acc-own")
		opened := openFor(t, a.id, "PC-HTTP-OWN")
		before := ledgerCounts(t, ctx, app)
		avail, reserved, version := accountBalanceRow(t, ctx, app, opened.Account.ID)

		list := env.do(http.MethodGet, "/api/v1/accounts", "", authz(a), nextTestIP())
		if list.Code != http.StatusOK {
			t.Fatalf("list = %d %s", list.Code, list.Body)
		}
		accounts := decodeAccounts(t, list.Body)
		if len(accounts) != 1 || accounts[0].ID != opened.Account.ID {
			t.Fatalf("list = %+v", accounts)
		}
		if accounts[0].AccountNumber != "PC-HTTP-OWN" || accounts[0].Purpose != "MAIN" ||
			accounts[0].Currency != "RUB" || accounts[0].Status != "ACTIVE" {
			t.Fatalf("list item = %+v", accounts[0])
		}
		assertNoTechFields(t, list.Body.String())

		get := env.do(http.MethodGet, "/api/v1/accounts/"+opened.Account.ID.String(), "", authz(a), nextTestIP())
		if get.Code != http.StatusOK {
			t.Fatalf("get = %d %s", get.Code, get.Body)
		}
		one := decodeAccount(t, get.Body)
		if one.ID != opened.Account.ID || one.AccountNumber != "PC-HTTP-OWN" {
			t.Fatalf("get = %+v", one)
		}
		assertNoTechFields(t, get.Body.String())

		bal := env.do(http.MethodGet, "/api/v1/accounts/"+opened.Account.ID.String()+"/balance", "", authz(a), nextTestIP())
		if bal.Code != http.StatusOK {
			t.Fatalf("balance = %d %s", bal.Code, bal.Body)
		}
		b := decodeBalance(t, bal.Body)
		if b.AccountID != opened.Account.ID || b.Currency != "RUB" {
			t.Fatalf("balance = %+v", b)
		}
		if b.Available != 0 || b.Reserved != 0 || b.Version != 0 {
			t.Fatalf("zero balance after open %+v", b)
		}

		stmt := env.do(http.MethodGet, "/api/v1/accounts/"+opened.Account.ID.String()+"/statement", "", authz(a), nextTestIP())
		if stmt.Code != http.StatusOK {
			t.Fatalf("statement = %d %s", stmt.Code, stmt.Body)
		}
		lines := decodeStatement(t, stmt.Body)
		if lines == nil || len(lines) != 0 {
			t.Fatalf("empty statement = %#v", lines)
		}
		if strings.Contains(stmt.Body.String(), "null") && strings.Contains(stmt.Body.String(), `"lines":null`) {
			t.Fatalf("null statement: %s", stmt.Body)
		}

		after := ledgerCounts(t, ctx, app)
		if after != before {
			t.Fatalf("reads mutated ledger: before=%+v after=%+v", before, after)
		}
		gotAvail, gotReserved, gotVersion := accountBalanceRow(t, ctx, app, opened.Account.ID)
		if gotAvail != avail || gotReserved != reserved || gotVersion != version {
			t.Fatalf("balance mutated %d %d %d -> %d %d %d", avail, reserved, version, gotAvail, gotReserved, gotVersion)
		}
	})

	t.Run("foreign account is 404", func(t *testing.T) {
		env.t = t
		a := registerLogin(t, "acc-a")
		b := registerLogin(t, "acc-b")
		accA := openFor(t, a.id, "PC-HTTP-A")
		accB := openFor(t, b.id, "PC-HTTP-B")

		for _, path := range []string{
			"/api/v1/accounts/" + accA.Account.ID.String(),
			"/api/v1/accounts/" + accA.Account.ID.String() + "/balance",
			"/api/v1/accounts/" + accA.Account.ID.String() + "/statement",
		} {
			rec := env.do(http.MethodGet, path, "", authz(b), nextTestIP())
			assertAuthError(t, rec, http.StatusNotFound, domain.CodeAccountNotFound)
			if strings.Contains(rec.Body.String(), "PC-HTTP-A") {
				t.Fatalf("leaked foreign number: %s", rec.Body)
			}
		}

		swap := env.do(http.MethodGet, "/api/v1/accounts/"+accB.Account.ID.String(), "", authz(a), nextTestIP())
		assertAuthError(t, swap, http.StatusNotFound, domain.CodeAccountNotFound)

		own := env.do(http.MethodGet, "/api/v1/accounts/"+accA.Account.ID.String(), "", authz(a), nextTestIP())
		if own.Code != http.StatusOK {
			t.Fatalf("owner get = %d %s", own.Code, own.Body)
		}
		listed := decodeAccounts(t, env.do(http.MethodGet, "/api/v1/accounts", "", authz(a), nextTestIP()).Body)
		if len(listed) != 1 || listed[0].ID != accA.Account.ID {
			t.Fatalf("A list leaked B: %+v", listed)
		}
	})

	t.Run("no token 401 other role 403 stale jwt", func(t *testing.T) {
		env.t = t
		a := registerLogin(t, "acc-rbac")
		openFor(t, a.id, "PC-HTTP-RBAC")

		missing := env.do(http.MethodGet, "/api/v1/accounts", "", "", nextTestIP())
		assertAuthError(t, missing, http.StatusUnauthorized, authdomain.CodeInvalidToken)

		actor := seedPGAdmin(t, ctx, app, &pgClock{t: time.Now().UTC()}, pgEmail(t, "acc-actor"))
		if err := application.GrantRoleIn(ctx, env.uow, env.write, adminPrincipal(actor), a.id, authdomain.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		stillCustomer := env.do(http.MethodGet, "/api/v1/accounts", "", authz(a), nextTestIP())
		if stillCustomer.Code != http.StatusOK {
			t.Fatalf("CUSTOMER+ADMIN store = %d %s", stillCustomer.Code, stillCustomer.Body)
		}
		if err := application.RevokeRoleIn(ctx, env.uow, env.write, adminPrincipal(actor), a.id, authdomain.RoleCustomer); err != nil {
			t.Fatal(err)
		}
		stale := env.do(http.MethodGet, "/api/v1/accounts", "", authz(a), nextTestIP())
		assertAuthError(t, stale, http.StatusForbidden, authdomain.CodeForbidden)
	})

	t.Run("bad uuid cursor limit unknown", func(t *testing.T) {
		env.t = t
		a := registerLogin(t, "acc-val")
		opened := openFor(t, a.id, "PC-HTTP-VAL")
		base := "/api/v1/accounts/" + opened.Account.ID.String() + "/statement"

		badID := env.do(http.MethodGet, "/api/v1/accounts/not-a-uuid", "", authz(a), nextTestIP())
		assertAuthError(t, badID, http.StatusBadRequest, authdomain.CodeInvalidRequest)

		for _, path := range []string{
			base + "?limit=0",
			base + "?limit=101",
			base + "?limit=abc",
			base + "?cursor=not-a-cursor",
		} {
			rec := env.do(http.MethodGet, path, "", authz(a), nextTestIP())
			assertAuthError(t, rec, http.StatusBadRequest, domain.CodeInvalidAccount)
		}

		missing := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
		unknown := env.do(http.MethodGet, "/api/v1/accounts/"+missing.String(), "", authz(a), nextTestIP())
		assertAuthError(t, unknown, http.StatusNotFound, domain.CodeAccountNotFound)
	})

	t.Run("post accounts not implemented", func(t *testing.T) {
		env.t = t
		a := registerLogin(t, "acc-post")
		rec := env.do(http.MethodPost, "/api/v1/accounts", `{"account_number":"PC-FAKE"}`, authz(a), nextTestIP())
		assertAuthError(t, rec, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
	})

	t.Run("statement after seeded topup does not write", func(t *testing.T) {
		env.t = t
		a := registerLogin(t, "acc-stmt")
		opened := openFor(t, a.id, "PC-HTTP-STMT")
		const amount int64 = 2500
		topupOpened(t, ctx, app, opened, amount)
		before := ledgerCounts(t, ctx, app)
		avail, reserved, version := accountBalanceRow(t, ctx, app, opened.Account.ID)

		rec := env.do(http.MethodGet, "/api/v1/accounts/"+opened.Account.ID.String()+"/statement?limit=10", "", authz(a), nextTestIP())
		if rec.Code != http.StatusOK {
			t.Fatalf("statement = %d %s", rec.Code, rec.Body)
		}
		lines := decodeStatement(t, rec.Body)
		if len(lines) != 1 || lines[0].Amount != amount || lines[0].Side != "CREDIT" {
			t.Fatalf("lines = %+v", lines)
		}
		raw := rec.Body.String()
		if strings.Contains(raw, "journal_id") || strings.Contains(raw, "ledger_account_id") {
			t.Fatalf("ledger identifiers leaked: %s", raw)
		}

		bal := env.do(http.MethodGet, "/api/v1/accounts/"+opened.Account.ID.String()+"/balance", "", authz(a), nextTestIP())
		b := decodeBalance(t, bal.Body)
		if b.Available != amount || b.Reserved != 0 {
			t.Fatalf("balance %+v", b)
		}

		after := ledgerCounts(t, ctx, app)
		if after != before {
			t.Fatalf("GET wrote ledger: before=%+v after=%+v", before, after)
		}
		gotAvail, gotReserved, gotVersion := accountBalanceRow(t, ctx, app, opened.Account.ID)
		if gotAvail != avail || gotReserved != reserved || gotVersion != version {
			t.Fatalf("GET mutated balance")
		}
	})
}

type accountsHTTPEnv struct {
	*authHTTPEnv
}

func newAccountsHTTPEnv(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *accountsHTTPEnv {
	t.Helper()
	signer, err := authcrypto.NewHS256([]byte(httpPGJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	base := application.Service{
		Hasher: authcrypto.NewArgon2idHasher(),
		Issuer: authcrypto.NewOpaqueRefresh(),
		Access: signer,
		Clock:  authcrypto.NewSystemClock(),
	}
	read := base
	read.Users = NewAuthUsers(pool)
	read.Sessions = NewAuthSessions(pool)
	read.Roles = NewAuthRoles(pool)
	uow := NewAuthUoW(pool)

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httpserver.New(config.HTTP{
		Addr:           "127.0.0.1:0",
		RequestTimeout: 60 * time.Second,
		MaxBodyBytes:   1 << 20,
	}, log)
	authhttp.Mount(srv, authhttp.New(uow, base, signer, read))
	accountshttp.Mount(srv, accountshttp.New(accountsapp.Service{
		Store:      NewAccounts(pool),
		Balances:   NewAccountBalances(pool),
		Statements: NewAccountStatements(pool),
	}, signer, read))

	inner := &authHTTPEnv{
		t:      t,
		ctx:    ctx,
		logs:   logs,
		h:      srv.Handler(),
		uow:    uow,
		write:  base,
		issuer: base.Issuer,
		banned: []string{
			httpPGPassword, httpPGJWTSecret, appPassword, ownerPassword,
			"Authorization", "$argon2id$", "pgx",
		},
	}
	return &accountsHTTPEnv{authHTTPEnv: inner}
}

type httpAccount struct {
	ID            uuid.UUID `json:"id"`
	AccountNumber string    `json:"account_number"`
	Purpose       string    `json:"purpose"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	Name          string    `json:"name"`
	CreatedAt     time.Time `json:"created_at"`
}

type httpBalance struct {
	AccountID uuid.UUID `json:"account_id"`
	Currency  string    `json:"currency"`
	Available int64     `json:"available"`
	Reserved  int64     `json:"reserved"`
	Version   int64     `json:"version"`
}

type httpStatementLine struct {
	Kind         string    `json:"kind"`
	Side         string    `json:"side"`
	Amount       int64     `json:"amount"`
	Currency     string    `json:"currency"`
	BalanceAfter *int64    `json:"balance_after"`
	CreatedAt    time.Time `json:"created_at"`
	PostedAt     time.Time `json:"posted_at"`
}

func decodeAccounts(t *testing.T, body *bytes.Buffer) []httpAccount {
	t.Helper()
	var env struct {
		Accounts []httpAccount `json:"accounts"`
	}
	dec := json.NewDecoder(bytes.NewReader(body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("accounts json: %v %s", err, body)
	}
	return env.Accounts
}

func decodeAccount(t *testing.T, body *bytes.Buffer) httpAccount {
	t.Helper()
	var acc httpAccount
	dec := json.NewDecoder(bytes.NewReader(body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&acc); err != nil {
		t.Fatalf("account json: %v %s", err, body)
	}
	return acc
}

func decodeBalance(t *testing.T, body *bytes.Buffer) httpBalance {
	t.Helper()
	var b httpBalance
	dec := json.NewDecoder(bytes.NewReader(body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		t.Fatalf("balance json: %v %s", err, body)
	}
	return b
}

func decodeStatement(t *testing.T, body *bytes.Buffer) []httpStatementLine {
	t.Helper()
	var env struct {
		Lines      []httpStatementLine `json:"lines"`
		NextCursor string              `json:"next_cursor"`
	}
	dec := json.NewDecoder(bytes.NewReader(body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("statement json: %v %s", err, body)
	}
	return env.Lines
}

func assertNoTechFields(t *testing.T, raw string) {
	t.Helper()
	for _, leak := range []string{`"user_id"`, `"merchant_id"`, `"version"`, `"updated_at"`, `"closed_at"`} {
		if strings.Contains(raw, leak) {
			t.Errorf("leaked %s in %s", leak, raw)
		}
	}
}

func accountBalanceRow(t *testing.T, ctx context.Context, conn *pgx.Conn, id uuid.UUID) (available, reserved, version int64) {
	t.Helper()
	if err := conn.QueryRow(ctx, `
		SELECT available_amount, reserved_amount, version
		  FROM ledger.account_balances WHERE account_id = $1`, id).
		Scan(&available, &reserved, &version); err != nil {
		t.Fatal(err)
	}
	return available, reserved, version
}
