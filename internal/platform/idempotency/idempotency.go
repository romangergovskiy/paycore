// Package idempotency is the HTTP idempotency protocol (D-07, concurrency.md §4).
//
// It does not import pgx. PostgreSQL adapters live in internal/platform/postgres.
package idempotency

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const (
	StatusInProgress = "IN_PROGRESS"
	StatusCompleted  = "COMPLETED"

	TTL = 24 * time.Hour

	CodeKeyReused  = "IDEMPOTENCY_KEY_REUSED"
	CodeInProgress = "REQUEST_IN_PROGRESS"
	CodeKeyInvalid = "INVALID_REQUEST"
)

// Error is a protocol error. Transport maps Code to HTTP.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code != "" && t.Code == e.Code
}

var (
	ErrKeyReused = &Error{
		Code:    CodeKeyReused,
		Message: "Idempotency key was already used with a different request.",
	}
	ErrInProgress = &Error{
		Code:    CodeInProgress,
		Message: "A request with this idempotency key is already in progress.",
	}
	ErrKeyInvalid = &Error{
		Code:    CodeKeyInvalid,
		Message: "Idempotency-Key must be 1 to 255 printable ASCII characters.",
	}
)

// Request is one HTTP-idempotent call. Scope is user:<id> or merchant:<id>.
type Request struct {
	ID       uuid.UUID
	Scope    string
	Endpoint string
	Key      string
	Hash     []byte
}

// Response is the HTTP result stored on COMPLETED. Body must be a safe
// representation: no passwords, refresh tokens, or API keys.
type Response struct {
	Status       int
	Body         json.RawMessage
	ResourceType string
	ResourceID   *uuid.UUID
}

// Result is the outcome of Execute. Replayed is true when the saved
// COMPLETED row was returned and the effect was not run.
type Result struct {
	Response Response
	Replayed bool
}

func UserScope(id uuid.UUID) string     { return "user:" + id.String() }
func MerchantScope(id uuid.UUID) string { return "merchant:" + id.String() }

// ValidKey reports whether k is 1–255 printable ASCII characters (D-07).
func ValidKey(k string) bool {
	if len(k) < 1 || len(k) > 255 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

func ValidScope(scope string) bool {
	const user, merchant = "user:", "merchant:"
	switch {
	case len(scope) > len(user) && scope[:len(user)] == user:
		_, err := uuid.Parse(scope[len(user):])
		return err == nil
	case len(scope) > len(merchant) && scope[:len(merchant)] == merchant:
		_, err := uuid.Parse(scope[len(merchant):])
		return err == nil
	default:
		return false
	}
}

func (r Request) Validate() error {
	if !ValidScope(r.Scope) {
		return ErrKeyInvalid
	}
	if len(r.Endpoint) < 1 || len(r.Endpoint) > 255 {
		return ErrKeyInvalid
	}
	if !ValidKey(r.Key) {
		return ErrKeyInvalid
	}
	if len(r.Hash) != 32 {
		return ErrKeyInvalid
	}
	return nil
}

// Persistable reports whether status may be stored on a COMPLETED key.
// 2xx and 4xx are stored; 5xx are not (concurrency.md §4.1).
func Persistable(status int) bool {
	return status >= 200 && status < 500
}
