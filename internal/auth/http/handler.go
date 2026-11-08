package authhttp

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

const opaqueRefreshRawLen = 32

// Handler is the Identity/Auth HTTP transport. It does not contain
// business rules: decode JSON, call application, map errors.
type Handler struct {
	register func(*http.Request, application.RegisterCommand) (domain.PublicUser, error)
	login    func(*http.Request, application.LoginCommand) (application.Credentials, error)
	refresh  func(*http.Request, application.RefreshCommand) (application.Credentials, error)
}

// New wires the public auth routes to application use cases.
func New(uow application.UnitOfWork, svc application.Service) Handler {
	return Handler{
		register: func(r *http.Request, cmd application.RegisterCommand) (domain.PublicUser, error) {
			return application.RegisterIn(r.Context(), uow, svc, cmd)
		},
		login: func(r *http.Request, cmd application.LoginCommand) (application.Credentials, error) {
			return application.LoginIn(r.Context(), uow, svc, cmd)
		},
		refresh: func(r *http.Request, cmd application.RefreshCommand) (application.Credentials, error) {
			return application.RefreshIn(r.Context(), uow, svc, cmd)
		},
	}
}

// Mount registers POST /api/v1/auth/register, login, and refresh.
func Mount(s *httpserver.Server, h Handler) {
	s.Handle("POST /api/v1/auth/register", http.HandlerFunc(h.postRegister))
	s.Handle("POST /api/v1/auth/login", http.HandlerFunc(h.postLogin))
	s.Handle("POST /api/v1/auth/refresh", http.HandlerFunc(h.postRefresh))
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
	if dec.More() {
		return invalidJSON()
	}
	return nil
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
