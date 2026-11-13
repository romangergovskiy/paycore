package authhttp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

func adminPrincipal() domain.Principal {
	return logoutPrincipal("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", domain.RoleAdmin, domain.RoleCustomer)
}

func customerPrincipal() domain.Principal {
	return logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", domain.RoleCustomer)
}

func targetID() uuid.UUID {
	return uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
}

func mountAdminHTTP(t *testing.T, sessions *logoutSessions) Handler {
	t.Helper()
	h := mountSelfService(t, sessions)
	h.listUsers = func(*http.Request, application.ListUsersQuery) (application.UserPage, error) {
		return application.UserPage{Users: []domain.PublicUser{}}, nil
	}
	h.grantRole = func(*http.Request, domain.Principal, uuid.UUID, domain.Role) error { return nil }
	h.revokeRole = func(*http.Request, domain.Principal, uuid.UUID, domain.Role) error { return nil }
	h.lockUser = func(*http.Request, application.StatusCommand) error { return nil }
	h.unlockUser = func(*http.Request, application.StatusCommand) error { return nil }
	h.disableUser = func(*http.Request, application.StatusCommand) error { return nil }
	h.enableUser = func(*http.Request, application.StatusCommand) error { return nil }
	return h
}

func adminBearer(t *testing.T) (Handler, domain.Principal, string) {
	t.Helper()
	p := adminPrincipal()
	h := mountAdminHTTP(t, newLogoutSessions(p))
	return h, p, signLogout(t, logoutSigner(t), p)
}

func adminHeader(tok string) http.Header {
	return http.Header{"Authorization": {"Bearer " + tok}}
}

func TestAdminEachOpRequiresAdmin(t *testing.T) {
	h, p, tok := adminBearer(t)
	uid := targetID()
	reason := `{"reason":"policy-violation"}`
	role := `{"role":"SUPPORT"}`
	ops := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/admin/users", ""},
		{http.MethodPost, "/api/v1/admin/users/" + uid.String() + "/roles", role},
		{http.MethodPost, "/api/v1/admin/users/" + uid.String() + "/roles/revoke", role},
		{http.MethodPost, "/api/v1/admin/users/" + uid.String() + "/lock", reason},
		{http.MethodPost, "/api/v1/admin/users/" + uid.String() + "/unlock", reason},
		{http.MethodPost, "/api/v1/admin/users/" + uid.String() + "/disable", reason},
		{http.MethodPost, "/api/v1/admin/users/" + uid.String() + "/enable", reason},
	}
	hdr := adminHeader(tok)
	for _, op := range ops {
		rec := serveAuth(t, h, op.method, op.path, op.body, hdr)
		if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
			t.Errorf("%s %s = %d %s", op.method, op.path, rec.Code, rec.Body)
		}
	}

	cust := customerPrincipal()
	sessions := newLogoutSessions(p, cust)
	h = mountAdminHTTP(t, sessions)
	custTok := signLogout(t, logoutSigner(t), cust)
	for _, op := range ops {
		rec := serveAuth(t, h, op.method, op.path, op.body, adminHeader(custTok))
		e := decodeErr(t, rec)
		if rec.Code != http.StatusForbidden || e.Error.Code != domain.CodeForbidden {
			t.Errorf("customer %s %s = %d %s", op.method, op.path, rec.Code, rec.Body)
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("403 must not send WWW-Authenticate: %s", op.path)
		}
	}

	for _, op := range ops {
		rec := serveAuth(t, h, op.method, op.path, op.body, nil)
		assertLogout401(t, rec)
	}
}

func TestAdminListKeysetAndSecrets(t *testing.T) {
	h, _, tok := adminBearer(t)
	t0 := time.Date(2026, 11, 13, 8, 0, 0, 123456789, time.UTC)
	first := domain.PublicUser{
		ID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), Email: "a@b.c",
		FirstName: "Ada", LastName: "Lovelace", Status: domain.StatusActive,
		Roles: []domain.Role{domain.RoleCustomer, domain.RoleAdmin}, CreatedAt: t0,
	}
	second := domain.PublicUser{
		ID: uuid.MustParse("22222222-2222-4222-8222-222222222222"), Email: "b@b.c",
		Status: domain.StatusLocked, Roles: []domain.Role{domain.RoleCustomer}, CreatedAt: t0.Add(time.Second),
	}
	var got application.ListUsersQuery
	h.listUsers = func(_ *http.Request, q application.ListUsersQuery) (application.UserPage, error) {
		got = q
		return application.UserPage{
			Users: []domain.PublicUser{first, second},
			Next:  &application.UserCursor{CreatedAt: second.CreatedAt, ID: second.ID},
		}, nil
	}

	rec := serveAuth(t, h, http.MethodGet, "/api/v1/admin/users", "", adminHeader(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if got.Limit != 50 || got.Status != nil || got.Cursor != nil {
		t.Fatalf("default query %+v", got)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	users, _ := env["users"].([]any)
	if len(users) != 2 {
		t.Fatalf("users = %#v", env)
	}
	row, _ := users[0].(map[string]any)
	if row["id"] != first.ID.String() || row["email"] != "a@b.c" || row["status"] != "ACTIVE" {
		t.Fatalf("row = %#v", row)
	}
	if _, ok := row["first_name"]; ok {
		t.Fatal("admin list must not include first_name")
	}
	if _, ok := row["password_hash"]; ok {
		t.Fatal("password_hash in JSON")
	}
	next, _ := env["next_cursor"].(string)
	if next == "" {
		t.Fatalf("next_cursor missing: %#v", env)
	}
	h.listUsers = func(_ *http.Request, q application.ListUsersQuery) (application.UserPage, error) {
		got = q
		return application.UserPage{Users: []domain.PublicUser{second}}, nil
	}
	page2 := serveAuth(t, h, http.MethodGet, "/api/v1/admin/users?limit=2&cursor="+next, "", adminHeader(tok))
	if page2.Code != http.StatusOK {
		t.Fatalf("page2 = %d %s", page2.Code, page2.Body)
	}
	if got.Cursor == nil || got.Cursor.ID != second.ID || !got.Cursor.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("decoded cursor %+v", got.Cursor)
	}
	raw := rec.Body.String()
	for _, leak := range []string{"password", "password_hash", "failed_login_count", "refresh_token", "access_token", "$argon2id$"} {
		if strings.Contains(raw, leak) {
			t.Errorf("leaked %s: %s", leak, raw)
		}
	}

	h.listUsers = func(_ *http.Request, q application.ListUsersQuery) (application.UserPage, error) {
		got = q
		return application.UserPage{Users: nil}, nil
	}
	empty := serveAuth(t, h, http.MethodGet, "/api/v1/admin/users?limit=10&status=LOCKED", "", adminHeader(tok))
	if empty.Code != http.StatusOK {
		t.Fatalf("empty = %d %s", empty.Code, empty.Body)
	}
	if got.Limit != 10 || got.Status == nil || *got.Status != domain.StatusLocked {
		t.Fatalf("filtered query %+v", got)
	}
	if !strings.Contains(empty.Body.String(), `"users":[]`) && !strings.Contains(empty.Body.String(), `"users": []`) {
		t.Fatalf("want empty array: %s", empty.Body)
	}
	if strings.Contains(empty.Body.String(), `"next_cursor"`) {
		t.Fatalf("next_cursor on last page: %s", empty.Body)
	}
}

func TestAdminListRejectsBadPagination(t *testing.T) {
	h, _, tok := adminBearer(t)
	h.listUsers = func(*http.Request, application.ListUsersQuery) (application.UserPage, error) {
		t.Fatal("list must not run")
		return application.UserPage{}, nil
	}
	hdr := adminHeader(tok)
	for _, path := range []string{
		"/api/v1/admin/users?limit=0",
		"/api/v1/admin/users?limit=101",
		"/api/v1/admin/users?limit=nope",
		"/api/v1/admin/users?status=NOPE",
		"/api/v1/admin/users?cursor=not-base64",
		"/api/v1/admin/users?cursor=e30",
		"/api/v1/admin/users?cursor=SELECT+1",
	} {
		rec := serveAuth(t, h, http.MethodGet, path, "", hdr)
		e := decodeErr(t, rec)
		if rec.Code != http.StatusBadRequest || e.Error.Code != domain.CodeInvalidRequest {
			t.Errorf("%s -> %d %s", path, rec.Code, rec.Body)
		}
		if rec.Code == http.StatusInternalServerError {
			t.Errorf("garbage cursor must not 500: %s", path)
		}
	}
}

func TestAdminActorFromPrincipalNotBody(t *testing.T) {
	h, p, tok := adminBearer(t)
	uid := targetID()
	var status application.StatusCommand
	var actor domain.Principal
	var role domain.Role
	h.lockUser = func(_ *http.Request, cmd application.StatusCommand) error {
		status = cmd
		return nil
	}
	h.grantRole = func(_ *http.Request, got domain.Principal, id uuid.UUID, r domain.Role) error {
		actor = got
		role = r
		if id != uid {
			t.Fatalf("id = %s", id)
		}
		return nil
	}
	hdr := adminHeader(tok)

	lock := serveAuth(t, h, http.MethodPost, "/api/v1/admin/users/"+uid.String()+"/lock",
		`{"reason":"abuse"}`, hdr)
	if lock.Code != http.StatusNoContent {
		t.Fatalf("lock = %d %s", lock.Code, lock.Body)
	}
	if status.Actor.UserID != p.UserID || status.Actor.SessionID != p.SessionID || status.UserID != uid || status.Reason != "abuse" {
		t.Fatalf("status cmd %+v want actor %s", status, p.UserID)
	}

	extra := serveAuth(t, h, http.MethodPost, "/api/v1/admin/users/"+uid.String()+"/lock",
		`{"reason":"abuse","actor_id":"11111111-1111-1111-1111-111111111111"}`, hdr)
	e := decodeErr(t, extra)
	if extra.Code != http.StatusBadRequest || e.Error.Code != domain.CodeInvalidRequest {
		t.Fatalf("actor_id body = %d %s", extra.Code, extra.Body)
	}

	grant := serveAuth(t, h, http.MethodPost, "/api/v1/admin/users/"+uid.String()+"/roles",
		`{"role":"SUPPORT"}`, hdr)
	if grant.Code != http.StatusNoContent {
		t.Fatalf("grant = %d %s", grant.Code, grant.Body)
	}
	if actor.UserID != p.UserID || role != domain.RoleSupport {
		t.Fatalf("grant actor %+v role %s", actor, role)
	}
}

func TestAdminInvalidUUIDAndDTO(t *testing.T) {
	h, _, tok := adminBearer(t)
	h.lockUser = func(*http.Request, application.StatusCommand) error {
		t.Fatal("lock must not run")
		return nil
	}
	h.grantRole = func(*http.Request, domain.Principal, uuid.UUID, domain.Role) error {
		t.Fatal("grant must not run")
		return nil
	}
	hdr := adminHeader(tok)
	badID := serveAuth(t, h, http.MethodPost, "/api/v1/admin/users/not-a-uuid/lock", `{"reason":"x"}`, hdr)
	e := decodeErr(t, badID)
	if badID.Code != http.StatusBadRequest || e.Error.Code != domain.CodeInvalidRequest {
		t.Fatalf("bad uuid = %d %s", badID.Code, badID.Body)
	}

	extra := serveAuth(t, h, http.MethodPost, "/api/v1/admin/users/"+targetID().String()+"/roles",
		`{"role":"SUPPORT","extra":true}`, hdr)
	ee := decodeErr(t, extra)
	if extra.Code != http.StatusBadRequest || ee.Error.Code != domain.CodeInvalidRequest {
		t.Fatalf("extra field = %d %s", extra.Code, extra.Body)
	}
}

func TestAdminDomainErrorsMapToHTTP(t *testing.T) {
	h, p, tok := adminBearer(t)
	hdr := adminHeader(tok)
	uid := targetID()
	path := "/api/v1/admin/users/" + uid.String()

	h.lockUser = func(*http.Request, application.StatusCommand) error { return domain.ErrNotFound }
	missing := serveAuth(t, h, http.MethodPost, path+"/lock", `{"reason":"x"}`, hdr)
	me := decodeErr(t, missing)
	if missing.Code != http.StatusNotFound || me.Error.Code != domain.CodeNotFound {
		t.Fatalf("not found = %d %s", missing.Code, missing.Body)
	}

	h.lockUser = func(*http.Request, application.StatusCommand) error { return domain.ErrLastAdmin }
	last := serveAuth(t, h, http.MethodPost, path+"/lock", `{"reason":"x"}`, hdr)
	le := decodeErr(t, last)
	if last.Code != http.StatusBadRequest || le.Error.Code != domain.CodeInvalidRequest {
		t.Fatalf("last admin = %d %s", last.Code, last.Body)
	}

	h.unlockUser = func(*http.Request, application.StatusCommand) error { return domain.ErrInvalidRequest }
	disabled := serveAuth(t, h, http.MethodPost, path+"/unlock", `{"reason":"x"}`, hdr)
	de := decodeErr(t, disabled)
	if disabled.Code != http.StatusBadRequest || de.Error.Code != domain.CodeInvalidRequest {
		t.Fatalf("unlock disabled = %d %s", disabled.Code, disabled.Body)
	}

	h.enableUser = func(*http.Request, application.StatusCommand) error { return nil }
	enable := serveAuth(t, h, http.MethodPost, path+"/enable", `{"reason":"restore"}`, hdr)
	if enable.Code != http.StatusNoContent {
		t.Fatalf("enable = %d %s", enable.Code, enable.Body)
	}

	h.revokeRole = func(_ *http.Request, actor domain.Principal, id uuid.UUID, role domain.Role) error {
		if actor.UserID == p.UserID && id == p.UserID && role == domain.RoleAdmin {
			return domain.ErrForbidden
		}
		return nil
	}
	self := serveAuth(t, h, http.MethodPost, "/api/v1/admin/users/"+p.UserID.String()+"/roles/revoke",
		`{"role":"ADMIN"}`, hdr)
	se := decodeErr(t, self)
	if self.Code != http.StatusForbidden || se.Error.Code != domain.CodeForbidden {
		t.Fatalf("self-revoke = %d %s", self.Code, self.Body)
	}

	h.grantRole = func(*http.Request, domain.Principal, uuid.UUID, domain.Role) error {
		return domain.ErrInvalidRequest
	}
	unknownRole := serveAuth(t, h, http.MethodPost, path+"/roles", `{"role":"ROOT"}`, hdr)
	ue := decodeErr(t, unknownRole)
	if unknownRole.Code != http.StatusBadRequest || ue.Error.Code != domain.CodeInvalidRequest {
		t.Fatalf("unknown role = %d %s", unknownRole.Code, unknownRole.Body)
	}
}

func TestAdminJWTRolesAreNotAuthorization(t *testing.T) {
	cust := customerPrincipal()
	cust.Roles = []domain.Role{domain.RoleCustomer}
	h := mountAdminHTTP(t, newLogoutSessions(cust))
	signedAsAdmin := cust
	signedAsAdmin.Roles = []domain.Role{domain.RoleAdmin, domain.RoleCustomer}
	tok := signLogout(t, logoutSigner(t), signedAsAdmin)
	rec := serveAuth(t, h, http.MethodGet, "/api/v1/admin/users", "", adminHeader(tok))
	e := decodeErr(t, rec)
	if rec.Code != http.StatusForbidden || e.Error.Code != domain.CodeForbidden {
		t.Fatalf("JWT.roles ADMIN with store CUSTOMER = %d %s", rec.Code, rec.Body)
	}
}

func TestAdminEnableLogsHaveNoSecrets(t *testing.T) {
	p := adminPrincipal()
	sessions := newLogoutSessions(p)
	h := mountAdminHTTP(t, sessions)
	tok := signLogout(t, logoutSigner(t), p)
	var logs bytes.Buffer
	s := httpserver.New(config.HTTP{Addr: "127.0.0.1:0", MaxBodyBytes: 1 << 20},
		slog.New(slog.NewJSONHandler(&logs, nil)))
	Mount(s, h)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/"+targetID().String()+"/enable",
		strings.NewReader(`{"reason":"restore"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.RemoteAddr = "203.0.113.10:54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	dump := logs.String() + rec.Body.String()
	for _, secret := range []string{tok, "Bearer " + tok, "password_hash", "$argon2id$"} {
		if strings.Contains(dump, secret) {
			t.Errorf("secret %q leaked", secret)
		}
	}
	if strings.Contains(logs.String(), `"Authorization"`) {
		t.Errorf("Authorization logged: %s", logs.String())
	}
}

func TestAdminCursorRoundTrip(t *testing.T) {
	cur := application.UserCursor{
		CreatedAt: time.Date(2026, 11, 13, 8, 0, 0, 123456789, time.UTC),
		ID:        uuid.MustParse("11111111-1111-4111-8111-111111111111"),
	}
	raw, err := encodeUserCursor(cur)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "+") || strings.Contains(raw, "/") || strings.Contains(raw, "=") {
		t.Fatalf("not URL-safe: %s", raw)
	}
	got, err := decodeUserCursor(raw)
	if err != nil || got == nil {
		t.Fatalf("decode: %v %v", got, err)
	}
	if got.ID != cur.ID || !got.CreatedAt.Equal(cur.CreatedAt) {
		t.Fatalf("got %+v want %+v", got, cur)
	}
}
