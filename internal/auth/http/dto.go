package authhttp

import (
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

func publicUserDTO(u domain.PublicUser) userDTO {
	roles := u.Roles
	if roles == nil {
		roles = []domain.Role{}
	}
	return userDTO{ID: u.ID, Email: u.Email, Roles: roles}
}
