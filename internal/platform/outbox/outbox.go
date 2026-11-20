// Package outbox is the transactional outbox contract (D-23, concurrency.md §7).
//
// It does not import pgx. PostgreSQL adapters live in internal/platform/postgres.
package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const (
	StatusPending   = "PENDING"
	StatusPublished = "PUBLISHED"
	StatusDead      = "DEAD"

	DefaultLease       = 30 * time.Second
	DefaultMaxAttempts = 8
	DefaultBatchSize   = 100
)

// Event is one outbox row. Seq is assigned by PostgreSQL IDENTITY on insert.
type Event struct {
	ID               uuid.UUID
	Seq              int64
	AggregateType    string
	AggregateID      uuid.UUID
	AggregateVersion int64
	Type             string
	Version          int16
	Payload          json.RawMessage
	Headers          json.RawMessage
	Status           string
	Attempts         int
	LastError        string
	CreatedAt        time.Time
	PublishedAt      *time.Time
}

// Publisher delivers one event outside a database transaction.
type Publisher interface {
	Publish(ctx context.Context, e Event) error
}
