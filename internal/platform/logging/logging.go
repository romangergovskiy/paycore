// Package logging configures log/slog and carries a request-scoped logger in
// context. These are technical logs; the audit log (spec §2.12) is a separate
// append-only table, not a log stream.
package logging

import (
	"context"
	"io"
	"log/slog"
)

type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

func New(w io.Writer, format Format, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == FormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

type ctxKey struct{}

func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger stored by WithLogger, or slog.Default().
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
