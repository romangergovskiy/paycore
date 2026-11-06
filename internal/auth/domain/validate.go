package domain

import (
	"strings"
	"unicode/utf8"
)

// CanonicalEmail lowercases and checks length and a single @.
func CanonicalEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if len(email) < MinEmailLen || len(email) > MaxEmailLen {
		return "", ErrInvalidRequest
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at != strings.LastIndexByte(email, '@') || at == len(email)-1 {
		return "", ErrInvalidRequest
	}
	return email, nil
}

func ValidatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if n < MinPasswordRunes || n > MaxPasswordRunes {
		return ErrInvalidRequest
	}
	return nil
}

func ValidateName(name string) error {
	if name == "" {
		return nil
	}
	n := utf8.RuneCountInString(name)
	if n < 1 || n > MaxNameRunes {
		return ErrInvalidRequest
	}
	return nil
}

func ValidateIP(ip string) error {
	if strings.TrimSpace(ip) == "" {
		return ErrInvalidRequest
	}
	return nil
}
