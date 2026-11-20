// Package accountshttp is the customer Accounts HTTP transport.
// It decodes requests, calls application use cases, and maps errors.
// Ownership is enforced in application/PostgreSQL, not after an unscoped Get.
package accountshttp

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	authdomain "github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

type Handler struct {
	list      func(*http.Request, uuid.UUID) ([]domain.Account, error)
	get       func(*http.Request, uuid.UUID, uuid.UUID) (domain.Account, error)
	balance   func(*http.Request, uuid.UUID, uuid.UUID) (domain.Account, int64, int64, int64, error)
	statement func(*http.Request, uuid.UUID, uuid.UUID, application.StatementQuery) (application.StatementPage, error)
	parser    httpserver.AccessParser
	sessions  httpserver.SessionValidator
}

// New wires customer account reads to application. parser and sessions are
// required; BearerAuth uses ValidateSession (store roles), not JWT.roles.
func New(svc application.Service, parser httpserver.AccessParser, sessions httpserver.SessionValidator) Handler {
	return Handler{
		list: func(r *http.Request, userID uuid.UUID) ([]domain.Account, error) {
			return svc.ListByOwner(r.Context(), &userID, nil)
		},
		get: func(r *http.Request, id, userID uuid.UUID) (domain.Account, error) {
			return svc.GetForOwner(r.Context(), id, userID)
		},
		balance: func(r *http.Request, id, userID uuid.UUID) (domain.Account, int64, int64, int64, error) {
			return svc.BalanceForOwner(r.Context(), id, userID)
		},
		statement: func(r *http.Request, id, userID uuid.UUID, q application.StatementQuery) (application.StatementPage, error) {
			return svc.StatementForOwner(r.Context(), id, userID, q)
		},
		parser:   parser,
		sessions: sessions,
	}
}

// Mount registers customer GET /api/v1/accounts routes. CUSTOMER is required.
// POST /accounts is not mounted (Q-13: account_number format is unset).
func Mount(s *httpserver.Server, h Handler) {
	if h.parser == nil || h.sessions == nil {
		return
	}
	authn := httpserver.BearerAuth(h.parser, h.sessions)
	customer := func(next http.Handler) http.Handler {
		return authn(httpserver.RequireRole(authdomain.RoleCustomer)(next))
	}
	s.Handle("GET /api/v1/accounts", customer(http.HandlerFunc(h.getAccounts)))
	s.Handle("GET /api/v1/accounts/{id}", customer(http.HandlerFunc(h.getAccount)))
	s.Handle("GET /api/v1/accounts/{id}/balance", customer(http.HandlerFunc(h.getBalance)))
	s.Handle("GET /api/v1/accounts/{id}/statement", customer(http.HandlerFunc(h.getStatement)))
}

func (h Handler) getAccounts(w http.ResponseWriter, r *http.Request) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	list, err := h.list(r, p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, accountsResponse{Accounts: accountDTOs(list)})
}

func (h Handler) getAccount(w http.ResponseWriter, r *http.Request) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	id, err := parsePathUUID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	acc, err := h.get(r, id, p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, accountDTOFrom(acc))
}

func (h Handler) getBalance(w http.ResponseWriter, r *http.Request) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	id, err := parsePathUUID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	acc, available, reserved, version, err := h.balance(r, id, p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, balanceResponse{
		AccountID: acc.ID,
		Currency:  string(acc.Currency),
		Available: available,
		Reserved:  reserved,
		Version:   version,
	})
}

func (h Handler) getStatement(w http.ResponseWriter, r *http.Request) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	id, err := parsePathUUID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	q, err := parseStatementQuery(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	page, err := h.statement(r, id, p.UserID, q)
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp := statementResponse{Lines: statementLineDTOs(page.Lines)}
	if page.NextCursor != nil {
		cur, err := encodeStatementCursor(*page.NextCursor)
		if err != nil {
			writeError(w, r, err)
			return
		}
		resp.NextCursor = cur
	}
	httpserver.WriteJSON(w, http.StatusOK, resp)
}

func principalOrUnauthorized(w http.ResponseWriter, r *http.Request) (authdomain.Principal, bool) {
	p, ok := httpserver.PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, authdomain.ErrInvalidToken)
		return authdomain.Principal{}, false
	}
	return p, true
}

func parsePathUUID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil, authdomain.ErrInvalidRequest
	}
	return id, nil
}

func parseStatementQuery(r *http.Request) (application.StatementQuery, error) {
	q := application.StatementQuery{Limit: application.DefaultStatementLimit}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > application.MaxStatementLimit {
			return application.StatementQuery{}, domain.ErrInvalidAccount
		}
		q.Limit = n
	}
	cur, err := decodeStatementCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return application.StatementQuery{}, err
	}
	q.Cursor = cur
	return q, nil
}
