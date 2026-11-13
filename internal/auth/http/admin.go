package authhttp

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

func (h Handler) mountAdmin(s *httpserver.Server, authn func(http.Handler) http.Handler) {
	admin := func(next http.Handler) http.Handler {
		return authn(httpserver.RequireRole(domain.RoleAdmin)(next))
	}
	if h.listUsers != nil {
		s.Handle("GET /api/v1/admin/users", admin(http.HandlerFunc(h.getAdminUsers)))
	}
	if h.grantRole != nil {
		s.Handle("POST /api/v1/admin/users/{id}/roles", admin(http.HandlerFunc(h.postAdminGrantRole)))
	}
	if h.revokeRole != nil {
		s.Handle("POST /api/v1/admin/users/{id}/roles/revoke", admin(http.HandlerFunc(h.postAdminRevokeRole)))
	}
	if h.lockUser != nil {
		s.Handle("POST /api/v1/admin/users/{id}/lock", admin(http.HandlerFunc(h.postAdminLock)))
	}
	if h.unlockUser != nil {
		s.Handle("POST /api/v1/admin/users/{id}/unlock", admin(http.HandlerFunc(h.postAdminUnlock)))
	}
	if h.disableUser != nil {
		s.Handle("POST /api/v1/admin/users/{id}/disable", admin(http.HandlerFunc(h.postAdminDisable)))
	}
	if h.enableUser != nil {
		s.Handle("POST /api/v1/admin/users/{id}/enable", admin(http.HandlerFunc(h.postAdminEnable)))
	}
}

func (h Handler) getAdminUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := principalOrUnauthorized(w, r); !ok {
		return
	}
	q, err := parseListUsersQuery(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	page, err := h.listUsers(r, q)
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp := adminUsersResponse{Users: adminUserDTOs(page.Users)}
	if page.Next != nil {
		cur, err := encodeUserCursor(*page.Next)
		if err != nil {
			writeError(w, r, err)
			return
		}
		resp.NextCursor = cur
	}
	httpserver.WriteJSON(w, http.StatusOK, resp)
}

func (h Handler) postAdminGrantRole(w http.ResponseWriter, r *http.Request) {
	h.postAdminRole(w, r, h.grantRole)
}

func (h Handler) postAdminRevokeRole(w http.ResponseWriter, r *http.Request) {
	h.postAdminRole(w, r, h.revokeRole)
}

func (h Handler) postAdminRole(w http.ResponseWriter, r *http.Request, fn func(*http.Request, domain.Principal, uuid.UUID, domain.Role) error) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	id, err := parsePathUUID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req roleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := fn(r, p, id, req.Role); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) postAdminLock(w http.ResponseWriter, r *http.Request) {
	h.postAdminStatus(w, r, h.lockUser)
}

func (h Handler) postAdminUnlock(w http.ResponseWriter, r *http.Request) {
	h.postAdminStatus(w, r, h.unlockUser)
}

func (h Handler) postAdminDisable(w http.ResponseWriter, r *http.Request) {
	h.postAdminStatus(w, r, h.disableUser)
}

func (h Handler) postAdminEnable(w http.ResponseWriter, r *http.Request) {
	h.postAdminStatus(w, r, h.enableUser)
}

func (h Handler) postAdminStatus(w http.ResponseWriter, r *http.Request, fn func(*http.Request, application.StatusCommand) error) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	id, err := parsePathUUID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req reasonRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := fn(r, application.StatusCommand{
		Actor:  p,
		UserID: id,
		Reason: req.Reason,
	}); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parsePathUUID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil, domain.ErrInvalidRequest
	}
	return id, nil
}

func parseListUsersQuery(r *http.Request) (application.ListUsersQuery, error) {
	q := application.ListUsersQuery{Limit: application.DefaultListUsersLimit}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < application.MinListUsersLimit || n > application.MaxListUsersLimit {
			return application.ListUsersQuery{}, domain.ErrInvalidRequest
		}
		q.Limit = n
	}
	if raw := r.URL.Query().Get("status"); raw != "" {
		st := domain.Status(raw)
		switch st {
		case domain.StatusActive, domain.StatusLocked, domain.StatusDisabled:
			q.Status = &st
		default:
			return application.ListUsersQuery{}, domain.ErrInvalidRequest
		}
	}
	cur, err := decodeUserCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return application.ListUsersQuery{}, err
	}
	q.Cursor = cur
	return q, nil
}
