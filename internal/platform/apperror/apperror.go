// Package apperror defines the application error type shared by all modules.
//
// An Error carries a Kind, which the transport layer maps to an HTTP status,
// a stable machine-readable Code in UPPER_SNAKE_CASE, a message that is safe
// to show to the client, and an optional wrapped cause that is only logged.
// Domain code never refers to HTTP.
//
// Every API error response has the same JSON body (decision D-39 in
// docs/architecture/decisions.md):
//
//	{
//	  "error": {
//	    "code": "INSUFFICIENT_FUNDS",
//	    "message": "Insufficient funds on the account.",
//	    "request_id": "XJ2N6V4B3QK7TZ5PLM8RWD4E9A"
//	  }
//	}
//
// For KindInternal the message is always generic; the cause never reaches the
// client.
package apperror

import "errors"

type Kind uint8

const (
	KindInternal Kind = iota
	KindInvalid
	KindUnauthorized
	KindForbidden
	KindNotFound
	KindMethodNotAllowed
	KindConflict
	KindTooLarge
	KindUnprocessable
	KindUnavailable
	KindTooManyRequests
)

func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid"
	case KindUnauthorized:
		return "unauthorized"
	case KindForbidden:
		return "forbidden"
	case KindNotFound:
		return "not_found"
	case KindMethodNotAllowed:
		return "method_not_allowed"
	case KindConflict:
		return "conflict"
	case KindTooLarge:
		return "too_large"
	case KindUnprocessable:
		return "unprocessable"
	case KindUnavailable:
		return "unavailable"
	case KindTooManyRequests:
		return "too_many_requests"
	default:
		return "internal"
	}
}

// Platform-level codes. Business modules define their own codes next to
// their domain errors.
const (
	CodeInternal         = "INTERNAL_ERROR"
	CodeInvalidRequest   = "INVALID_REQUEST"
	CodeNotFound         = "NOT_FOUND"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodeRequestTooLarge  = "REQUEST_TOO_LARGE"
	CodeRequestTimeout   = "REQUEST_TIMEOUT"
	CodeUnavailable      = "SERVICE_UNAVAILABLE"
)

const internalMessage = "Internal server error."

type Error struct {
	Kind    Kind
	Code    string
	Message string
	Err     error
}

func New(kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message}
}

// Wrap attaches a cause to a new Error. The cause is kept for logging and for
// errors.Is/As, but is not part of the client-facing message.
func Wrap(err error, kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message, Err: err}
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Message + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Is reports a match when target is an *Error with the same Code, so callers
// can compare against sentinel errors declared with New.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code != "" && t.Code == e.Code
}

// From returns the first *Error in err's chain, or an internal error wrapping
// err when there is none. It returns nil for a nil err.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Wrap(err, KindInternal, CodeInternal, internalMessage)
}

// PublicMessage is the message that may be sent to the client.
func (e *Error) PublicMessage() string {
	if e.Kind == KindInternal {
		return internalMessage
	}
	return e.Message
}
