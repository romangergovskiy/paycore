package postgres

import (
	"fmt"
	"net/url"
	"strings"
)

func parseDSN(dsn string) (*url.URL, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
	default:
		return nil, fmt.Errorf("parse DSN: scheme must be postgres or postgresql")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("parse DSN: host is required")
	}
	if strings.Trim(u.Path, "/") == "" {
		return nil, fmt.Errorf("parse DSN: database name is required")
	}
	return u, nil
}

func dbName(u *url.URL) string {
	return strings.Trim(u.Path, "/")
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func withDatabase(u *url.URL, name string) string {
	cp := *u
	cp.Path = "/" + name
	return cp.String()
}

func withUser(u *url.URL, user, password string) string {
	cp := *u
	cp.User = url.UserPassword(user, password)
	return cp.String()
}

// RedactDSN returns dsn with the password replaced by ***.
func RedactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "postgres://***"
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	out := u.Scheme + "://"
	if user != "" {
		out += user + ":***@"
	}
	out += u.Host + u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}
