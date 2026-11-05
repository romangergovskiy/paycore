package domain

// Codes match D-30 / TЗ error names. HTTP Kind is applied outside this package.
const (
	CodeInvalidAccount       = "INVALID_ACCOUNT"
	CodeCurrencyNotSupported = "CURRENCY_NOT_SUPPORTED"
	CodeAccountNotFound      = "ACCOUNT_NOT_FOUND"
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
	ErrInvalidAccount       = &Error{Code: CodeInvalidAccount, Message: "Account is invalid."}
	ErrCurrencyNotSupported = &Error{
		Code: CodeCurrencyNotSupported, Message: "Currency is not supported.",
	}
	ErrAccountNotFound = &Error{Code: CodeAccountNotFound, Message: "Account was not found."}
)
