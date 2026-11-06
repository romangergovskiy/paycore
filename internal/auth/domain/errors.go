package domain

const (
	CodeInvalidRequest     = "INVALID_REQUEST"
	CodeEmailTaken         = "EMAIL_TAKEN"
	CodeInvalidCredentials = "INVALID_CREDENTIALS"
	CodeInvalidToken       = "INVALID_TOKEN"
	CodeTokenExpired       = "TOKEN_EXPIRED"
	CodeTokenReused        = "TOKEN_REUSED"
	CodeForbidden          = "FORBIDDEN"
	CodeNotFound           = "NOT_FOUND"
	CodeRateLimited        = "AUTH_RATE_LIMITED"
)

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && e != nil && t.Code != "" && t.Code == e.Code
}

var (
	ErrInvalidRequest     = &Error{Code: CodeInvalidRequest, Message: "Request is invalid."}
	ErrEmailTaken         = &Error{Code: CodeEmailTaken, Message: "Email is already registered."}
	ErrInvalidCredentials = &Error{
		Code: CodeInvalidCredentials, Message: "Invalid email or password.",
	}
	ErrInvalidToken = &Error{Code: CodeInvalidToken, Message: "Token is invalid."}
	ErrTokenExpired = &Error{Code: CodeTokenExpired, Message: "Token has expired."}
	ErrTokenReused  = &Error{Code: CodeTokenReused, Message: "Refresh token was reused."}
	ErrForbidden    = &Error{Code: CodeForbidden, Message: "Action is not allowed."}
	ErrNotFound     = &Error{Code: CodeNotFound, Message: "User was not found."}
	ErrRateLimited  = &Error{Code: CodeRateLimited, Message: "Too many attempts. Retry later."}
	ErrLastAdmin    = &Error{Code: CodeInvalidRequest, Message: "The last ADMIN cannot be revoked."}
)
