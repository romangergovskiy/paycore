package authhttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

const opaqueRefreshRawLen = 32

type profileReader interface {
	CurrentUser(context.Context, domain.Principal) (domain.PublicUser, error)
	ListSessions(context.Context, domain.Principal) ([]domain.Session, error)
}

// Handler is the Identity/Auth HTTP transport. It does not contain
// business rules: decode JSON, call application, map errors.
type Handler struct {
	register       func(*http.Request, application.RegisterCommand) (domain.PublicUser, error)
	login          func(*http.Request, application.LoginCommand) (application.Credentials, error)
	refresh        func(*http.Request, application.RefreshCommand) (application.Credentials, error)
	logout         func(*http.Request, domain.Principal) error
	logoutAll      func(*http.Request, domain.Principal) error
	changePassword func(*http.Request, application.ChangePasswordCommand) error
	revokeSession  func(*http.Request, application.RevokeSessionCommand) error
	currentUser    func(*http.Request, domain.Principal) (domain.PublicUser, error)
	listSessions   func(*http.Request, domain.Principal) ([]domain.Session, error)
	parser         httpserver.AccessParser
	sessions       httpserver.SessionValidator
}

// New wires auth routes to application use cases. parser and sessions
// are required to mount Bearer routes (logout, me, logout-all, password,
// sessions). Logout and Refresh share uow with the other writes.
func New(uow application.UnitOfWork, svc application.Service, parser httpserver.AccessParser, sessions httpserver.SessionValidator) Handler {
	return Compose(uow, uow, svc, parser, sessions)
}

// Compose is New, except logoutRefresh is the UnitOfWork for Logout and
// Refresh only. Production passes a one-shot 40P01/40001 retry wrapper
// there; Register, Login, LogoutAll, ChangePassword, and RevokeSession
// keep uow. logoutRefresh nil uses uow.
func Compose(uow, logoutRefresh application.UnitOfWork, svc application.Service, parser httpserver.AccessParser, sessions httpserver.SessionValidator) Handler {
	if logoutRefresh == nil {
		logoutRefresh = uow
	}
	h := Handler{
		register: func(r *http.Request, cmd application.RegisterCommand) (domain.PublicUser, error) {
			return application.RegisterIn(r.Context(), uow, svc, cmd)
		},
		login: func(r *http.Request, cmd application.LoginCommand) (application.Credentials, error) {
			return application.LoginIn(r.Context(), uow, svc, cmd)
		},
		refresh: func(r *http.Request, cmd application.RefreshCommand) (application.Credentials, error) {
			return application.RefreshIn(r.Context(), logoutRefresh, svc, cmd)
		},
		logout: func(r *http.Request, p domain.Principal) error {
			return application.LogoutIn(r.Context(), logoutRefresh, svc, p)
		},
		logoutAll: func(r *http.Request, p domain.Principal) error {
			return application.LogoutAllIn(r.Context(), uow, svc, p)
		},
		changePassword: func(r *http.Request, cmd application.ChangePasswordCommand) error {
			return application.ChangePasswordIn(r.Context(), uow, svc, cmd)
		},
		revokeSession: func(r *http.Request, cmd application.RevokeSessionCommand) error {
			return application.RevokeSessionIn(r.Context(), uow, svc, cmd)
		},
		parser:   parser,
		sessions: sessions,
	}
	if reader, ok := sessions.(profileReader); ok {
		h.currentUser = func(r *http.Request, p domain.Principal) (domain.PublicUser, error) {
			return reader.CurrentUser(r.Context(), p)
		}
		h.listSessions = func(r *http.Request, p domain.Principal) ([]domain.Session, error) {
			return reader.ListSessions(r.Context(), p)
		}
	}
	return h
}

// Mount registers POST /api/v1/auth/register, login, and refresh.
// Bearer routes are mounted only when BearerAuth dependencies are present.
// Self-service routes also require their use-case func (tests may set a
// subset).
func Mount(s *httpserver.Server, h Handler) {
	s.Handle("POST /api/v1/auth/register", http.HandlerFunc(h.postRegister))
	s.Handle("POST /api/v1/auth/login", http.HandlerFunc(h.postLogin))
	s.Handle("POST /api/v1/auth/refresh", http.HandlerFunc(h.postRefresh))
	if h.parser == nil || h.sessions == nil {
		return
	}
	authn := httpserver.BearerAuth(h.parser, h.sessions)
	s.Handle("POST /api/v1/auth/logout", authn(http.HandlerFunc(h.postLogout)))
	if h.currentUser != nil {
		s.Handle("GET /api/v1/auth/me", authn(http.HandlerFunc(h.getMe)))
	}
	if h.logoutAll != nil {
		s.Handle("POST /api/v1/auth/logout-all", authn(http.HandlerFunc(h.postLogoutAll)))
	}
	if h.changePassword != nil {
		s.Handle("POST /api/v1/auth/password", authn(http.HandlerFunc(h.postPassword)))
	}
	if h.listSessions != nil {
		s.Handle("GET /api/v1/auth/sessions", authn(http.HandlerFunc(h.getSessions)))
	}
	if h.revokeSession != nil {
		s.Handle("POST /api/v1/auth/sessions/{id}/revoke", authn(http.HandlerFunc(h.postRevokeSession)))
	}
}

func (h Handler) postRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	user, err := h.register(r, application.RegisterCommand{
		Email:     req.Email,
		Password:  req.Password,
		FirstName: req.FirstName,
		LastName:  req.LastName,
		IP:        clientIP(r),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, userEnvelope{User: publicUserDTO(user)})
}

func (h Handler) postLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	cred, err := h.login(r, application.LoginCommand{
		Email:     req.Email,
		Password:  req.Password,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, credentialsDTO(cred))
}

func (h Handler) postRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.RefreshToken == "" {
		writeError(w, r, domain.ErrInvalidRequest)
		return
	}
	if !opaqueRefreshShape(req.RefreshToken) {
		writeError(w, r, domain.ErrInvalidToken)
		return
	}
	cred, err := h.refresh(r, application.RefreshCommand{
		RefreshToken: req.RefreshToken,
		IP:           clientIP(r),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, credentialsDTO(cred))
}

func (h Handler) postLogout(w http.ResponseWriter, r *http.Request) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	if err := h.logout(r, p); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) getMe(w http.ResponseWriter, r *http.Request) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	u, err := h.currentUser(r, p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, profileDTO(u))
}

func (h Handler) postLogoutAll(w http.ResponseWriter, r *http.Request) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	if err := h.logoutAll(r, p); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) postPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	var req passwordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := h.changePassword(r, application.ChangePasswordCommand{
		Actor:       p,
		OldPassword: req.OldPassword,
		NewPassword: req.NewPassword,
	}); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) getSessions(w http.ResponseWriter, r *http.Request) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	list, err := h.listSessions(r, p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, sessionsResponse{Sessions: sessionDTOs(list, p.SessionID)})
}

func (h Handler) postRevokeSession(w http.ResponseWriter, r *http.Request) {
	p, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, domain.ErrInvalidRequest)
		return
	}
	if err := h.revokeSession(r, application.RevokeSessionCommand{
		Actor:     p,
		SessionID: id,
	}); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func principalOrUnauthorized(w http.ResponseWriter, r *http.Request) (domain.Principal, bool) {
	p, ok := httpserver.PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, domain.ErrInvalidToken)
		return domain.Principal{}, false
	}
	return p, true
}

func credentialsDTO(c application.Credentials) credentialsResponse {
	return credentialsResponse{
		AccessToken:  c.AccessToken,
		TokenType:    c.TokenType,
		ExpiresIn:    c.ExpiresIn,
		RefreshToken: c.RefreshToken,
		User:         publicUserDTO(c.User),
	}
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	// A second value (err == nil) or junk after the object is INVALID_REQUEST.
	return invalidJSON()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func opaqueRefreshShape(token string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(raw) == opaqueRefreshRawLen
}
