package authhttp

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

type registerRequest struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type passwordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

type roleRequest struct {
	Role domain.Role `json:"role"`
}

type userCursorJSON struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

type adminUserDTO struct {
	ID        uuid.UUID     `json:"id"`
	Email     string        `json:"email"`
	Status    domain.Status `json:"status"`
	Roles     []domain.Role `json:"roles"`
	CreatedAt time.Time     `json:"created_at"`
}

type adminUsersResponse struct {
	Users      []adminUserDTO `json:"users"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

type userDTO struct {
	ID    uuid.UUID     `json:"id"`
	Email string        `json:"email"`
	Roles []domain.Role `json:"roles"`
}

type userEnvelope struct {
	User userDTO `json:"user"`
}

type credentialsResponse struct {
	AccessToken  string  `json:"access_token"`
	TokenType    string  `json:"token_type"`
	ExpiresIn    int     `json:"expires_in"`
	RefreshToken string  `json:"refresh_token"`
	User         userDTO `json:"user"`
}

type meResponse struct {
	ID        uuid.UUID     `json:"id"`
	Email     string        `json:"email"`
	FirstName string        `json:"first_name"`
	LastName  string        `json:"last_name"`
	Status    domain.Status `json:"status"`
	Roles     []domain.Role `json:"roles"`
}

type sessionDTO struct {
	ID         uuid.UUID `json:"id"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Current    bool      `json:"current"`
}

type sessionsResponse struct {
	Sessions []sessionDTO `json:"sessions"`
}

func publicUserDTO(u domain.PublicUser) userDTO {
	roles := u.Roles
	if roles == nil {
		roles = []domain.Role{}
	}
	return userDTO{ID: u.ID, Email: u.Email, Roles: roles}
}

func profileDTO(u domain.PublicUser) meResponse {
	roles := u.Roles
	if roles == nil {
		roles = []domain.Role{}
	}
	return meResponse{
		ID:        u.ID,
		Email:     u.Email,
		FirstName: u.FirstName,
		LastName:  u.LastName,
		Status:    u.Status,
		Roles:     roles,
	}
}

func sessionDTOs(in []domain.Session, current uuid.UUID) []sessionDTO {
	out := make([]sessionDTO, 0, len(in))
	for _, s := range in {
		out = append(out, sessionDTO{
			ID:         s.ID,
			UserAgent:  s.UserAgent,
			IP:         s.IP,
			CreatedAt:  s.CreatedAt,
			LastSeenAt: s.LastSeenAt,
			Current:    s.ID == current,
		})
	}
	return out
}

func adminUserDTOs(in []domain.PublicUser) []adminUserDTO {
	out := make([]adminUserDTO, 0, len(in))
	for _, u := range in {
		roles := u.Roles
		if roles == nil {
			roles = []domain.Role{}
		}
		out = append(out, adminUserDTO{
			ID:        u.ID,
			Email:     u.Email,
			Status:    u.Status,
			Roles:     roles,
			CreatedAt: u.CreatedAt,
		})
	}
	return out
}

func encodeUserCursor(c application.UserCursor) (string, error) {
	payload, err := json.Marshal(userCursorJSON{
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339Nano),
		ID:        c.ID.String(),
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeUserCursor(raw string) (*application.UserCursor, error) {
	if raw == "" {
		return nil, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, domain.ErrInvalidRequest
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.DisallowUnknownFields()
	var body userCursorJSON
	if err := dec.Decode(&body); err != nil {
		return nil, domain.ErrInvalidRequest
	}
	created, err := time.Parse(time.RFC3339Nano, body.CreatedAt)
	if err != nil {
		return nil, domain.ErrInvalidRequest
	}
	id, err := uuid.Parse(body.ID)
	if err != nil || id == uuid.Nil {
		return nil, domain.ErrInvalidRequest
	}
	return &application.UserCursor{CreatedAt: created.UTC(), ID: id}, nil
}
