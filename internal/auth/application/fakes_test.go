package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

type frozenClock struct{ t time.Time }

func (c *frozenClock) Now() time.Time { return c.t }

type memHasher struct {
	failHash, failVerify bool
	lastVerify           string
	verifyCalls          int
}

func (h *memHasher) Hash(password string) (string, error) {
	if h.failHash {
		return "", errors.New("hash failed")
	}
	return "$argon2id$v=19$m=19456,t=2,p=1$fake$" + password, nil
}

func (h *memHasher) Verify(password, encoded string) (bool, error) {
	h.verifyCalls++
	h.lastVerify = encoded
	if h.failVerify {
		return false, errors.New("verify failed")
	}
	if encoded == h.DummyHash() {
		return false, nil
	}
	want, err := h.Hash(password)
	if err != nil {
		return false, err
	}
	return encoded == want, nil
}

func (h *memHasher) DummyHash() string {
	return "$argon2id$v=19$m=19456,t=2,p=1$dummy$xxxxxxxxxxxxxxxx"
}

type memIssuer struct {
	n        int
	fail     bool
	failHash bool
}

func (i *memIssuer) Issue() (string, []byte, error) {
	if i.fail {
		return "", nil, errors.New("refresh issue failed")
	}
	i.n++
	plain := fmt.Sprintf("refresh-%d", i.n)
	sum := sha256.Sum256([]byte(plain))
	return plain, sum[:], nil
}

func (i *memIssuer) Hash(plaintext string) ([]byte, error) {
	if i.failHash {
		return nil, errors.New("refresh hash failed")
	}
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:], nil
}

type memSigner struct {
	fail  bool
	calls int
	last  domain.Principal
	exp   time.Time
}

func (s *memSigner) Sign(p domain.Principal, now, exp time.Time) (string, error) {
	s.calls++
	s.last = p
	s.exp = exp
	if s.fail {
		return "", errors.New("sign failed")
	}
	return fmt.Sprintf("access:%s:%s:%d", p.UserID, p.SessionID, exp.Unix()), nil
}

type memLimiter struct {
	mu  sync.Mutex
	n   map[string]int
	err error
}

func (l *memLimiter) Hit(ctx context.Context, bucket domain.RateBucket, key string, at time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if l.err != nil {
		return 0, l.err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == nil {
		l.n = map[string]int{}
	}
	window := at.UTC().Truncate(domain.RateWindow)
	k := string(bucket) + "|" + key + "|" + window.Format(time.RFC3339)
	l.n[k]++
	return l.n[k], nil
}

func (l *memLimiter) count(bucket domain.RateBucket, key string, at time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	window := at.UTC().Truncate(domain.RateWindow)
	k := string(bucket) + "|" + key + "|" + window.Format(time.RFC3339)
	return l.n[k]
}

type memUsers struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]domain.User
	create error
	update error
}

func (s *memUsers) Create(ctx context.Context, u domain.User) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.create != nil {
		return s.create
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = map[uuid.UUID]domain.User{}
	}
	for _, got := range s.byID {
		if got.Email == u.Email {
			return domain.ErrEmailTaken
		}
	}
	s.byID[u.ID] = u
	return nil
}

func (s *memUsers) Get(ctx context.Context, id uuid.UUID) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u, nil
}

func (s *memUsers) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.byID {
		if u.Email == email {
			return u, nil
		}
	}
	return domain.User{}, domain.ErrNotFound
}

func (s *memUsers) GetForUpdate(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return s.Get(ctx, id)
}

func (s *memUsers) Update(ctx context.Context, u domain.User) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.update != nil {
		return s.update
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[u.ID]; !ok {
		return domain.ErrNotFound
	}
	s.byID[u.ID] = u
	return nil
}

type memSessions struct {
	mu   sync.Mutex
	byID map[uuid.UUID]domain.Session
	err  error
}

func (s *memSessions) Create(ctx context.Context, sess domain.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = map[uuid.UUID]domain.Session{}
	}
	s.byID[sess.ID] = sess
	return nil
}

func (s *memSessions) Get(ctx context.Context, id uuid.UUID) (domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return domain.Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return domain.Session{}, domain.ErrNotFound
	}
	return sess, nil
}

func (s *memSessions) Revoke(ctx context.Context, id uuid.UUID, reason domain.RevokeReason, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	if sess.RevokedAt == nil {
		t := at
		sess.RevokedAt = &t
		sess.RevokeReason = reason
		s.byID[id] = sess
	}
	return nil
}

func (s *memSessions) RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason domain.RevokeReason, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.byID {
		if sess.UserID == userID && sess.RevokedAt == nil {
			t := at
			sess.RevokedAt = &t
			sess.RevokeReason = reason
			s.byID[id] = sess
		}
	}
	return nil
}

func (s *memSessions) Touch(ctx context.Context, id uuid.UUID, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	sess.LastSeenAt = at
	s.byID[id] = sess
	return nil
}

type memTokens struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]domain.RefreshToken
	sessions *memSessions
	create   error
}

func (s *memTokens) Create(ctx context.Context, tok domain.RefreshToken) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.create != nil {
		return s.create
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = map[uuid.UUID]domain.RefreshToken{}
	}
	tok.Hash = copyHash(tok.Hash)
	s.byID[tok.ID] = tok
	return nil
}

func (s *memTokens) lookup(hash []byte) (domain.RefreshToken, bool) {
	for _, tok := range s.byID {
		if string(tok.Hash) == string(hash) {
			return tok, true
		}
	}
	return domain.RefreshToken{}, false
}

func (s *memTokens) GetByHash(ctx context.Context, hash []byte) (domain.RefreshToken, error) {
	if err := ctx.Err(); err != nil {
		return domain.RefreshToken{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.lookup(hash)
	if !ok {
		return domain.RefreshToken{}, domain.ErrNotFound
	}
	return tok, nil
}

func (s *memTokens) GetForUpdate(ctx context.Context, hash []byte) (domain.RefreshToken, error) {
	return s.GetByHash(ctx, hash)
}

func (s *memTokens) MarkUsed(ctx context.Context, id, replacedBy uuid.UUID, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	t := at
	rid := replacedBy
	tok.UsedAt = &t
	tok.ReplacedBy = &rid
	s.byID[id] = tok
	return nil
}

func (s *memTokens) RevokeBySession(ctx context.Context, sessionID uuid.UUID, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, tok := range s.byID {
		if tok.SessionID == sessionID && tok.RevokedAt == nil {
			t := at
			tok.RevokedAt = &t
			s.byID[id] = tok
		}
	}
	return nil
}

func (s *memTokens) RevokeAllForUser(ctx context.Context, userID uuid.UUID, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	owned := map[uuid.UUID]struct{}{}
	if s.sessions != nil {
		s.sessions.mu.Lock()
		for id, sess := range s.sessions.byID {
			if sess.UserID == userID {
				owned[id] = struct{}{}
			}
		}
		s.sessions.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, tok := range s.byID {
		if _, ok := owned[tok.SessionID]; !ok {
			continue
		}
		if tok.RevokedAt == nil {
			t := at
			tok.RevokedAt = &t
			s.byID[id] = tok
		}
	}
	return nil
}

type memRoles struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]domain.UserRole
	users   *memUsers
	grant   error
	lockErr error
	calls   []string
}

func (s *memRoles) note(op string) {
	s.mu.Lock()
	s.calls = append(s.calls, op)
	s.mu.Unlock()
}

func (s *memRoles) LockAdminRoleMutations(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.lockErr != nil {
		return s.lockErr
	}
	s.note("lock")
	return nil
}

func (s *memRoles) Grant(ctx context.Context, ur domain.UserRole) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.grant != nil {
		return s.grant
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "grant")
	if s.byUser == nil {
		s.byUser = map[uuid.UUID][]domain.UserRole{}
	}
	for _, got := range s.byUser[ur.UserID] {
		if got.Role == ur.Role {
			return domain.ErrInvalidRequest
		}
	}
	s.byUser[ur.UserID] = append(s.byUser[ur.UserID], ur)
	return nil
}

func (s *memRoles) Revoke(ctx context.Context, userID uuid.UUID, role domain.Role) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "revoke")
	keep := s.byUser[userID][:0]
	for _, ur := range s.byUser[userID] {
		if ur.Role != role {
			keep = append(keep, ur)
		}
	}
	s.byUser[userID] = keep
	return nil
}

func (s *memRoles) List(ctx context.Context, userID uuid.UUID) ([]domain.Role, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Role, 0, len(s.byUser[userID]))
	for _, ur := range s.byUser[userID] {
		out = append(out, ur.Role)
	}
	return out, nil
}

func (s *memRoles) Has(ctx context.Context, userID uuid.UUID, role domain.Role) (bool, error) {
	roles, err := s.List(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, r := range roles {
		if r == role {
			return true, nil
		}
	}
	return false, nil
}

func (s *memRoles) CountActiveWithRole(ctx context.Context, role domain.Role) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.note("count")
	s.mu.Lock()
	ids := make([]uuid.UUID, 0)
	for uid, urs := range s.byUser {
		for _, ur := range urs {
			if ur.Role == role {
				ids = append(ids, uid)
				break
			}
		}
	}
	s.mu.Unlock()
	n := 0
	for _, id := range ids {
		u, err := s.users.Get(ctx, id)
		if err != nil {
			continue
		}
		if u.Status == domain.StatusActive {
			n++
		}
	}
	return n, nil
}

type memAttempts struct {
	mu   sync.Mutex
	all  []domain.LoginAttempt
	fail error
}

func (s *memAttempts) Append(ctx context.Context, a domain.LoginAttempt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.fail != nil {
		return s.fail
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all = append(s.all, a)
	return nil
}

type world struct {
	txMu     sync.Mutex
	users    *memUsers
	sessions *memSessions
	tokens   *memTokens
	roles    *memRoles
	attempts *memAttempts
	limiter  *memLimiter
	hasher   *memHasher
	issuer   *memIssuer
	signer   *memSigner
	clock    *frozenClock
}

type worldSnap struct {
	users    map[uuid.UUID]domain.User
	sessions map[uuid.UUID]domain.Session
	tokens   map[uuid.UUID]domain.RefreshToken
	roles    map[uuid.UUID][]domain.UserRole
	attempts []domain.LoginAttempt
	limiter  map[string]int
	issuerN  int
}

func newWorld() *world {
	users := &memUsers{byID: map[uuid.UUID]domain.User{}}
	roles := &memRoles{byUser: map[uuid.UUID][]domain.UserRole{}, users: users}
	sessions := &memSessions{byID: map[uuid.UUID]domain.Session{}}
	return &world{
		users:    users,
		sessions: sessions,
		tokens:   &memTokens{byID: map[uuid.UUID]domain.RefreshToken{}, sessions: sessions},
		roles:    roles,
		attempts: &memAttempts{},
		limiter:  &memLimiter{n: map[string]int{}},
		hasher:   &memHasher{},
		issuer:   &memIssuer{},
		signer:   &memSigner{},
		clock:    &frozenClock{t: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)},
	}
}

func (w *world) service() Service {
	return Service{
		Users:    w.users,
		Sessions: w.sessions,
		Tokens:   w.tokens,
		Roles:    w.roles,
		Attempts: w.attempts,
		Limiter:  w.limiter,
		Hasher:   w.hasher,
		Issuer:   w.issuer,
		Access:   w.signer,
		Clock:    w.clock,
	}
}

func (w *world) deps() Deps {
	return Deps{
		Users:    w.users,
		Sessions: w.sessions,
		Tokens:   w.tokens,
		Roles:    w.roles,
		Attempts: w.attempts,
		Limiter:  w.limiter,
	}
}

type memUoW struct {
	w         *world
	committed bool
	rolled    bool
}

func (u *memUoW) Do(ctx context.Context, fn func(context.Context, Deps) (Decision, error)) error {
	u.committed = false
	u.rolled = false
	if err := ctx.Err(); err != nil {
		u.rolled = true
		return err
	}
	u.w.txMu.Lock()
	defer u.w.txMu.Unlock()
	snap := u.w.snapshot()
	dec, err := fn(ctx, u.w.deps())
	if ctx.Err() != nil {
		u.w.restore(snap)
		u.rolled = true
		return ctx.Err()
	}
	if dec != Commit {
		u.w.restore(snap)
		u.rolled = true
		return err
	}
	u.committed = true
	return err
}

func (w *world) snapshot() worldSnap {
	w.users.mu.Lock()
	users := make(map[uuid.UUID]domain.User, len(w.users.byID))
	for k, v := range w.users.byID {
		users[k] = cloneUser(v)
	}
	w.users.mu.Unlock()

	w.sessions.mu.Lock()
	sessions := make(map[uuid.UUID]domain.Session, len(w.sessions.byID))
	for k, v := range w.sessions.byID {
		sessions[k] = cloneSession(v)
	}
	w.sessions.mu.Unlock()

	w.tokens.mu.Lock()
	tokens := make(map[uuid.UUID]domain.RefreshToken, len(w.tokens.byID))
	for k, v := range w.tokens.byID {
		tokens[k] = cloneToken(v)
	}
	w.tokens.mu.Unlock()

	w.roles.mu.Lock()
	roles := make(map[uuid.UUID][]domain.UserRole, len(w.roles.byUser))
	for k, v := range w.roles.byUser {
		roles[k] = append([]domain.UserRole(nil), v...)
	}
	w.roles.mu.Unlock()

	w.attempts.mu.Lock()
	attempts := append([]domain.LoginAttempt(nil), w.attempts.all...)
	w.attempts.mu.Unlock()

	w.limiter.mu.Lock()
	limiter := make(map[string]int, len(w.limiter.n))
	for k, v := range w.limiter.n {
		limiter[k] = v
	}
	w.limiter.mu.Unlock()

	return worldSnap{
		users:    users,
		sessions: sessions,
		tokens:   tokens,
		roles:    roles,
		attempts: attempts,
		limiter:  limiter,
		issuerN:  w.issuer.n,
	}
}

func (w *world) restore(s worldSnap) {
	w.users.mu.Lock()
	w.users.byID = s.users
	w.users.mu.Unlock()

	w.sessions.mu.Lock()
	w.sessions.byID = s.sessions
	w.sessions.mu.Unlock()

	w.tokens.mu.Lock()
	w.tokens.byID = s.tokens
	w.tokens.mu.Unlock()

	w.roles.mu.Lock()
	w.roles.byUser = s.roles
	w.roles.mu.Unlock()

	w.attempts.mu.Lock()
	w.attempts.all = s.attempts
	w.attempts.mu.Unlock()

	w.limiter.mu.Lock()
	w.limiter.n = s.limiter
	w.limiter.mu.Unlock()

	w.issuer.n = s.issuerN
}

func cloneUser(u domain.User) domain.User {
	if u.LockedAt != nil {
		t := *u.LockedAt
		u.LockedAt = &t
	}
	return u
}

func cloneSession(s domain.Session) domain.Session {
	if s.RevokedAt != nil {
		t := *s.RevokedAt
		s.RevokedAt = &t
	}
	return s
}

func cloneToken(tok domain.RefreshToken) domain.RefreshToken {
	tok.Hash = copyHash(tok.Hash)
	if tok.ReplacedBy != nil {
		id := *tok.ReplacedBy
		tok.ReplacedBy = &id
	}
	if tok.UsedAt != nil {
		t := *tok.UsedAt
		tok.UsedAt = &t
	}
	if tok.RevokedAt != nil {
		t := *tok.RevokedAt
		tok.RevokedAt = &t
	}
	return tok
}

func registerCmd(email string) RegisterCommand {
	return RegisterCommand{
		Email:     email,
		Password:  "twelvechars!",
		FirstName: "Ada",
		LastName:  "Lovelace",
		IP:        "203.0.113.10",
	}
}

func loginCmd(email string) LoginCommand {
	return LoginCommand{
		Email:     email,
		Password:  "twelvechars!",
		IP:        "203.0.113.10",
		UserAgent: "paycore-test",
	}
}

func seedUser(t interface{ Fatal(...any) }, w *world, email string, status domain.Status, roles ...domain.Role) domain.User {
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	now := w.clock.t
	u := domain.User{
		ID:           id,
		Email:        email,
		PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$fake$twelvechars!",
		FirstName:    "Pat",
		LastName:     "Admin",
		Status:       status,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if status == domain.StatusLocked {
		t := now
		u.LockedAt = &t
	}
	if err := w.users.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if err := w.roles.Grant(context.Background(), domain.UserRole{
			UserID:    id,
			Role:      r,
			GrantedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

func adminPrincipal(u domain.User) domain.Principal {
	return domain.Principal{UserID: u.ID, Roles: []domain.Role{domain.RoleAdmin}}
}
