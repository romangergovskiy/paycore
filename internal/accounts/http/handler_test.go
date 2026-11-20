package accountshttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	authdomain "github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
	"github.com/romangergovskiy/paycore/internal/platform/authcrypto"
	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
	"github.com/romangergovskiy/paycore/pkg/money"
)

const (
	testJWTSecret    = "0123456789abcdef0123456789abcdef"
	testCursorSecret = "paycore-accounts-cursor-secret!!"
)

type fakeSessions struct {
	mu   sync.Mutex
	byID map[uuid.UUID]authdomain.Principal
}

func newFakeSessions(ps ...authdomain.Principal) *fakeSessions {
	f := &fakeSessions{byID: make(map[uuid.UUID]authdomain.Principal, len(ps))}
	for _, p := range ps {
		f.byID[p.SessionID] = p
	}
	return f
}

func (f *fakeSessions) ValidateSession(_ context.Context, sessionID uuid.UUID) (authdomain.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.byID[sessionID]
	if !ok {
		return authdomain.Principal{}, authdomain.ErrInvalidToken
	}
	return p, nil
}

func (f *fakeSessions) setRoles(sessionID uuid.UUID, roles []authdomain.Role) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.byID[sessionID]
	if !ok {
		return
	}
	p.Roles = append([]authdomain.Role(nil), roles...)
	f.byID[sessionID] = p
}

func testPrincipal(roles ...authdomain.Role) authdomain.Principal {
	if len(roles) == 0 {
		roles = []authdomain.Role{authdomain.RoleCustomer}
	}
	return authdomain.Principal{
		UserID:    uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		SessionID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:     roles,
	}
}

func testCursor(t *testing.T) *CursorCodec {
	t.Helper()
	c, err := NewCursorCodec([]byte(testCursorSecret))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testSigner(t *testing.T) *authcrypto.HS256 {
	t.Helper()
	s, err := authcrypto.NewHS256([]byte(testJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func signToken(t *testing.T, s *authcrypto.HS256, p authdomain.Principal) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	tok, err := s.Sign(p, now, now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func testAccount(owner uuid.UUID) domain.Account {
	created := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	uid := owner
	return domain.Account{
		ID:            uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001"),
		AccountNumber: "PC-HTTP-1",
		UserID:        &uid,
		Purpose:       domain.PurposeMain,
		Currency:      money.RUB,
		Status:        domain.StatusActive,
		Name:          "Main",
		Version:       3,
		CreatedAt:     created,
		UpdatedAt:     created,
	}
}

func newTestHandler(t *testing.T, p authdomain.Principal) (Handler, string) {
	t.Helper()
	acc := testAccount(p.UserID)
	sessions := newFakeSessions(p)
	h := Handler{
		list: func(*http.Request, uuid.UUID) ([]domain.Account, error) {
			return []domain.Account{acc}, nil
		},
		get: func(_ *http.Request, id, userID uuid.UUID) (domain.Account, error) {
			if id != acc.ID || userID != p.UserID {
				return domain.Account{}, domain.ErrAccountNotFound
			}
			return acc, nil
		},
		balance: func(_ *http.Request, id, userID uuid.UUID) (domain.Account, int64, int64, int64, error) {
			if id != acc.ID || userID != p.UserID {
				return domain.Account{}, 0, 0, 0, domain.ErrAccountNotFound
			}
			return acc, 1500, 200, 3, nil
		},
		statement: func(_ *http.Request, id, userID uuid.UUID, q application.StatementQuery) (application.StatementPage, error) {
			if id != acc.ID || userID != p.UserID {
				return application.StatementPage{}, domain.ErrAccountNotFound
			}
			if err := validateLimit(q); err != nil {
				return application.StatementPage{}, err
			}
			after := int64(1500)
			return application.StatementPage{
				Lines: []application.StatementLine{{
					ID:              uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000001"),
					JournalID:       uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001"),
					LedgerAccountID: uuid.MustParse("eeeeeeee-eeee-4eee-8eee-000000000001"),
					Kind:            ledger.KindAvailable,
					Side:            ledger.Credit,
					Amount:          1500,
					Currency:        "RUB",
					BalanceAfter:    &after,
					CreatedAt:       acc.CreatedAt,
					PostedAt:        acc.CreatedAt,
				}},
			}, nil
		},
		parser:   testSigner(t),
		sessions: sessions,
		cursors:  testCursor(t),
	}
	return h, signToken(t, testSigner(t), p)
}

func validateLimit(q application.StatementQuery) error {
	if q.Limit < 1 || q.Limit > application.MaxStatementLimit {
		return domain.ErrInvalidAccount
	}
	return nil
}

func serveAccounts(t *testing.T, h Handler, method, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	s := httpserver.New(config.HTTP{Addr: "127.0.0.1:0", MaxBodyBytes: 1 << 20},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	Mount(s, h)
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "203.0.113.10:54321"
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func bearer(tok string) http.Header {
	return http.Header{"Authorization": {"Bearer " + tok}}
}

type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q body = %s", ct, rec.Body)
	}
	var env errorEnvelope
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("decode error: %v body=%s", err, rec.Body)
	}
	if env.Error.Code == "" || env.Error.Message == "" || env.Error.RequestID == "" {
		t.Fatalf("incomplete D-39 body: %+v", env.Error)
	}
	return env
}

func TestListAccountsSuccess(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts", bearer(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	list, ok := body["accounts"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("body = %s", rec.Body)
	}
	acc := list[0].(map[string]any)
	if acc["id"] != "aaaaaaaa-aaaa-4aaa-8aaa-000000000001" || acc["account_number"] != "PC-HTTP-1" {
		t.Fatalf("account = %#v", acc)
	}
	if acc["purpose"] != "MAIN" || acc["currency"] != "RUB" || acc["status"] != "ACTIVE" || acc["name"] != "Main" {
		t.Fatalf("fields = %#v", acc)
	}
	for _, leak := range []string{"version", "user_id", "merchant_id", "updated_at", "closed_at"} {
		if _, ok := acc[leak]; ok {
			t.Errorf("leaked %s", leak)
		}
	}
}

func TestListAccountsEmpty(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	h.list = func(*http.Request, uuid.UUID) ([]domain.Account, error) {
		return []domain.Account{}, nil
	}
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts", bearer(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Accounts []accountDTO `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Accounts == nil || len(body.Accounts) != 0 {
		t.Fatalf("empty list = %#v", body.Accounts)
	}
	if strings.Contains(rec.Body.String(), "null") {
		t.Fatalf("null list: %s", rec.Body)
	}
}

func TestListUsesPrincipalUserIDNotQuery(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	var got uuid.UUID
	h.list = func(_ *http.Request, userID uuid.UUID) ([]domain.Account, error) {
		got = userID
		return []domain.Account{}, nil
	}
	other := uuid.MustParse("99999999-9999-4999-8999-999999999999")
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts?user_id="+other.String(), bearer(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if got != p.UserID {
		t.Fatalf("userID = %s want principal %s", got, p.UserID)
	}
}

func TestGetAccountSuccess(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	acc := testAccount(p.UserID)
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts/"+acc.ID.String(), bearer(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != acc.ID.String() || body["account_number"] != "PC-HTTP-1" {
		t.Fatalf("body = %#v", body)
	}
	for _, leak := range []string{"version", "user_id", "merchant_id", "updated_at", "closed_at"} {
		if _, ok := body[leak]; ok {
			t.Errorf("leaked %s", leak)
		}
	}
}

func TestGetBalanceSuccess(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	acc := testAccount(p.UserID)
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts/"+acc.ID.String()+"/balance", bearer(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["account_id"] != acc.ID.String() || body["currency"] != "RUB" {
		t.Fatalf("body = %#v", body)
	}
	if body["available"].(float64) != 1500 || body["reserved"].(float64) != 200 || body["version"].(float64) != 3 {
		t.Fatalf("money = %#v", body)
	}
	raw := rec.Body.String()
	if strings.Contains(raw, ".") && strings.Contains(raw, "1500.0") {
		t.Fatalf("float money: %s", raw)
	}
}

func TestGetStatementSuccessOmitsLedgerIDs(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	acc := testAccount(p.UserID)
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts/"+acc.ID.String()+"/statement", bearer(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	lines, ok := body["lines"].([]any)
	if !ok || len(lines) != 1 {
		t.Fatalf("body = %s", rec.Body)
	}
	line := lines[0].(map[string]any)
	if line["kind"] != "AVAILABLE" || line["side"] != "CREDIT" || line["amount"].(float64) != 1500 {
		t.Fatalf("line = %#v", line)
	}
	for _, leak := range []string{"id", "journal_id", "ledger_account_id", "account_version"} {
		if _, ok := line[leak]; ok {
			t.Errorf("leaked %s", leak)
		}
	}
	raw := rec.Body.String()
	for _, id := range []string{
		"cccccccc-cccc-4ccc-8ccc-000000000001",
		"dddddddd-dddd-4ddd-8ddd-000000000001",
		"eeeeeeee-eeee-4eee-8eee-000000000001",
	} {
		if strings.Contains(raw, id) {
			t.Fatalf("ledger id leaked: %s", raw)
		}
	}
}

func TestGetStatementEmpty(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	h.statement = func(*http.Request, uuid.UUID, uuid.UUID, application.StatementQuery) (application.StatementPage, error) {
		return application.StatementPage{Lines: []application.StatementLine{}}, nil
	}
	acc := testAccount(p.UserID)
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts/"+acc.ID.String()+"/statement", bearer(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Lines []statementLineDTO `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Lines == nil || len(body.Lines) != 0 {
		t.Fatalf("empty statement = %#v", body.Lines)
	}
}

func TestNoTokenUnauthorized(t *testing.T) {
	p := testPrincipal()
	h, _ := newTestHandler(t, p)
	paths := []string{
		"/api/v1/accounts",
		"/api/v1/accounts/" + testAccount(p.UserID).ID.String(),
		"/api/v1/accounts/" + testAccount(p.UserID).ID.String() + "/balance",
		"/api/v1/accounts/" + testAccount(p.UserID).ID.String() + "/statement",
	}
	for _, path := range paths {
		rec := serveAccounts(t, h, http.MethodGet, path, nil)
		e := decodeErr(t, rec)
		if rec.Code != http.StatusUnauthorized || e.Error.Code != authdomain.CodeInvalidToken {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body)
		}
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s missing WWW-Authenticate", path)
		}
	}
}

func TestOtherRoleForbidden(t *testing.T) {
	p := testPrincipal(authdomain.RoleAdmin)
	h, tok := newTestHandler(t, p)
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts", bearer(tok))
	e := decodeErr(t, rec)
	if rec.Code != http.StatusForbidden || e.Error.Code != authdomain.CodeForbidden {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
}

func TestStaleJWTRoleIsNotAuthorization(t *testing.T) {
	store := testPrincipal(authdomain.RoleCustomer)
	signed := store
	signed.Roles = []authdomain.Role{authdomain.RoleAdmin}
	sessions := newFakeSessions(store)
	h, _ := newTestHandler(t, store)
	h.sessions = sessions
	h.parser = testSigner(t)
	tok := signToken(t, testSigner(t), signed)

	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts", bearer(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("JWT ADMIN / store CUSTOMER = %d %s", rec.Code, rec.Body)
	}

	sessions.setRoles(store.SessionID, []authdomain.Role{authdomain.RoleAdmin})
	denied := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts", bearer(tok))
	e := decodeErr(t, denied)
	if denied.Code != http.StatusForbidden || e.Error.Code != authdomain.CodeForbidden {
		t.Fatalf("stale CUSTOMER JWT / store ADMIN = %d %s", denied.Code, denied.Body)
	}
}

func TestBadUUID(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts/not-a-uuid", bearer(tok))
	e := decodeErr(t, rec)
	if rec.Code != http.StatusBadRequest || e.Error.Code != authdomain.CodeInvalidRequest {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
}

func TestUnknownAccount(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	missing := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
	for _, path := range []string{
		"/api/v1/accounts/" + missing.String(),
		"/api/v1/accounts/" + missing.String() + "/balance",
		"/api/v1/accounts/" + missing.String() + "/statement",
	} {
		rec := serveAccounts(t, h, http.MethodGet, path, bearer(tok))
		e := decodeErr(t, rec)
		if rec.Code != http.StatusNotFound || e.Error.Code != domain.CodeAccountNotFound {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
}

func TestForeignAccountIsNotFound(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	acc := testAccount(p.UserID)
	var gotUser uuid.UUID
	h.get = func(_ *http.Request, id, userID uuid.UUID) (domain.Account, error) {
		gotUser = userID
		if userID != p.UserID || id != acc.ID {
			return domain.Account{}, domain.ErrAccountNotFound
		}
		return acc, nil
	}
	foreign := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-000000000002")
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts/"+foreign.String(), bearer(tok))
	e := decodeErr(t, rec)
	if rec.Code != http.StatusNotFound || e.Error.Code != domain.CodeAccountNotFound {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if gotUser != p.UserID {
		t.Fatalf("handler used user %s", gotUser)
	}
}

func TestStatementHTTPSignedCursorPagination(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	acc := testAccount(p.UserID)
	id1 := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000001")
	id2 := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000002")
	t1 := acc.CreatedAt
	t2 := acc.CreatedAt.Add(time.Second)
	var got []*application.StatementCursor
	h.statement = func(_ *http.Request, _, _ uuid.UUID, q application.StatementQuery) (application.StatementPage, error) {
		cp := q.Cursor
		if cp != nil {
			v := *cp
			got = append(got, &v)
		} else {
			got = append(got, nil)
		}
		if q.Cursor == nil {
			return application.StatementPage{
				Lines: []application.StatementLine{{
					ID: id1, Kind: ledger.KindAvailable, Side: ledger.Credit, Amount: 1, Currency: "RUB",
					CreatedAt: t1, PostedAt: t1,
				}},
				NextCursor: &application.StatementCursor{CreatedAt: t1, ID: id1},
			}, nil
		}
		return application.StatementPage{
			Lines: []application.StatementLine{{
				ID: id2, Kind: ledger.KindAvailable, Side: ledger.Credit, Amount: 2, Currency: "RUB",
				CreatedAt: t2, PostedAt: t2,
			}},
		}, nil
	}

	base := "/api/v1/accounts/" + acc.ID.String() + "/statement?limit=1"
	first := serveAccounts(t, h, http.MethodGet, base, bearer(tok))
	if first.Code != http.StatusOK {
		t.Fatalf("first = %d %s", first.Code, first.Body)
	}
	var page struct {
		Lines      []statementLineDTO `json:"lines"`
		NextCursor string             `json:"next_cursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 1 || page.NextCursor == "" {
		t.Fatalf("first page %+v", page)
	}
	if strings.Contains(page.NextCursor, id1.String()) {
		t.Fatalf("cursor leaked uuid: %s", page.NextCursor)
	}

	second := serveAccounts(t, h, http.MethodGet, base+"&cursor="+page.NextCursor, bearer(tok))
	if second.Code != http.StatusOK {
		t.Fatalf("second = %d %s", second.Code, second.Body)
	}
	page = struct {
		Lines      []statementLineDTO `json:"lines"`
		NextCursor string             `json:"next_cursor"`
	}{}
	if err := json.Unmarshal(second.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 1 || page.Lines[0].Amount != 2 || page.NextCursor != "" {
		t.Fatalf("second page %+v", page)
	}
	if len(got) != 2 || got[0] != nil || got[1] == nil || got[1].ID != id1 {
		t.Fatalf("cursors seen %+v", got)
	}
	if got[1].ID == id2 {
		t.Fatal("duplicate/skip")
	}
}

func TestStatementHTTPTamperedCursor(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	acc := testAccount(p.UserID)
	raw, err := h.cursors.Encode(application.StatementCursor{
		CreatedAt: acc.CreatedAt, ID: acc.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	bin, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	bin[len(bin)-1] ^= 0x01
	tampered := base64.RawURLEncoding.EncodeToString(bin)
	rec := serveAccounts(t, h, http.MethodGet, "/api/v1/accounts/"+acc.ID.String()+"/statement?cursor="+tampered, bearer(tok))
	e := decodeErr(t, rec)
	if rec.Code != http.StatusBadRequest || e.Error.Code != domain.CodeInvalidAccount {
		t.Fatalf("tampered = %d %s", rec.Code, rec.Body)
	}
}

func TestBadCursorAndLimit(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	acc := testAccount(p.UserID)
	base := "/api/v1/accounts/" + acc.ID.String() + "/statement"
	legacy, err := json.Marshal(map[string]string{
		"created_at": acc.CreatedAt.UTC().Format(time.RFC3339Nano),
		"id":         acc.ID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		base + "?limit=0",
		base + "?limit=101",
		base + "?limit=abc",
		base + "?limit=-1",
		base + "?cursor=not-a-cursor",
		base + "?cursor=not-base64",
		base + "?cursor=" + base64.RawURLEncoding.EncodeToString(legacy),
		base + "?cursor=" + strings.Repeat("A", 129),
	} {
		rec := serveAccounts(t, h, http.MethodGet, path, bearer(tok))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body.String())
			continue
		}
		e := decodeErr(t, rec)
		if e.Error.Code != domain.CodeInvalidAccount {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestPostAccountsNotMounted(t *testing.T) {
	p := testPrincipal()
	h, tok := newTestHandler(t, p)
	rec := serveAccounts(t, h, http.MethodPost, "/api/v1/accounts", bearer(tok))
	e := decodeErr(t, rec)
	if rec.Code != http.StatusMethodNotAllowed || e.Error.Code != "METHOD_NOT_ALLOWED" {
		t.Fatalf("POST /accounts = %d %s", rec.Code, rec.Body)
	}
}

func TestNewWiresUseCases(t *testing.T) {
	h := New(application.Service{}, testSigner(t), newFakeSessions(), testCursor(t))
	if h.list == nil || h.get == nil || h.balance == nil || h.statement == nil || h.cursors == nil {
		t.Fatal("New left a use case unset")
	}
}

func TestMountWithoutParser(t *testing.T) {
	s := httpserver.New(config.HTTP{Addr: "127.0.0.1:0", MaxBodyBytes: 1 << 20},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	Mount(s, Handler{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unmounted = %d", rec.Code)
	}
}

func TestMountWithoutCursorCodec(t *testing.T) {
	p := testPrincipal()
	h, _ := newTestHandler(t, p)
	h.cursors = nil
	s := httpserver.New(config.HTTP{Addr: "127.0.0.1:0", MaxBodyBytes: 1 << 20},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	Mount(s, h)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unmounted without codec = %d", rec.Code)
	}
}
