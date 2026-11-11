package authhttp

import (
	"time"

	"github.com/google/uuid"

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
