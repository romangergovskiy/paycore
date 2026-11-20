package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/platform/outbox"
)

// InsertOutboxEvent writes a PENDING event in the caller's transaction
// (the same Tx as the business effect and the idempotency key).
func InsertOutboxEvent(ctx context.Context, tx pgx.Tx, e outbox.Event) (outbox.Event, error) {
	if e.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return e, err
		}
		e.ID = id
	}
	if e.Version == 0 {
		e.Version = 1
	}
	payload := jsonOrEmpty(e.Payload)
	headers := jsonOrEmpty(e.Headers)
	err := tx.QueryRow(ctx, `
		INSERT INTO platform.outbox_events (
		    id, aggregate_type, aggregate_id, aggregate_version,
		    event_type, event_version, payload, headers, status, next_attempt_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, 'PENDING', now())
		RETURNING seq, created_at`,
		e.ID, e.AggregateType, e.AggregateID, e.AggregateVersion,
		e.Type, e.Version, string(payload), string(headers),
	).Scan(&e.Seq, &e.CreatedAt)
	if err != nil {
		return e, fmt.Errorf("outbox insert: %w", err)
	}
	e.Status = outbox.StatusPending
	e.Payload = payload
	e.Headers = headers
	return e, nil
}

func jsonOrEmpty(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(`{}`)
	}
	return b
}

// OutboxRelay claims, ACKs, and fails outbox rows. Publish happens outside
// any transaction (D-23). There is no cmd/worker in Stage A; tests call these
// functions directly.
type OutboxRelay struct {
	db          beginner
	Owner       string
	Lease       time.Duration
	MaxAttempts int
	BatchSize   int
}

func NewOutboxRelay(db beginner, owner string) *OutboxRelay {
	return &OutboxRelay{
		db:          db,
		Owner:       owner,
		Lease:       outbox.DefaultLease,
		MaxAttempts: outbox.DefaultMaxAttempts,
		BatchSize:   outbox.DefaultBatchSize,
	}
}

// Claim takes a short transaction: FOR UPDATE SKIP LOCKED, lease, COMMIT.
// A later event of the same aggregate is not selected while an earlier
// PENDING or DEAD row exists. Whether DEAD should unblock later seq is not
// a locked product decision; Stage A does not auto-advance.
func (r *OutboxRelay) Claim(ctx context.Context) ([]outbox.Event, error) {
	if r.Owner == "" {
		return nil, errors.New("outbox claim: lease owner is required")
	}
	lease := r.Lease
	if lease <= 0 {
		lease = outbox.DefaultLease
	}
	limit := r.BatchSize
	if limit <= 0 {
		limit = outbox.DefaultBatchSize
	}
	var events []outbox.Event
	err := doTx(ctx, r.db, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH batch AS (
			    SELECT o.id
			      FROM platform.outbox_events o
			     WHERE o.status = 'PENDING'
			       AND o.next_attempt_at <= now()
			       AND (o.processing_until IS NULL OR o.processing_until < now())
			       AND NOT EXISTS (
			             SELECT 1
			               FROM platform.outbox_events p
			              WHERE p.aggregate_id = o.aggregate_id
			                AND p.seq < o.seq
			                AND p.status IN ('PENDING', 'DEAD')
			           )
			     ORDER BY o.seq
			     LIMIT $3
			     FOR UPDATE SKIP LOCKED
			)
			UPDATE platform.outbox_events o
			   SET processing_until = now() + $2::interval,
			       lease_owner = $1,
			       attempts = attempts + 1
			  FROM batch
			 WHERE o.id = batch.id
			 RETURNING o.id, o.seq, o.aggregate_type, o.aggregate_id, o.aggregate_version,
			           o.event_type, o.event_version, o.payload, o.headers, o.attempts`,
			r.Owner, lease.String(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e outbox.Event
			if err := rows.Scan(
				&e.ID, &e.Seq, &e.AggregateType, &e.AggregateID, &e.AggregateVersion,
				&e.Type, &e.Version, &e.Payload, &e.Headers, &e.Attempts,
			); err != nil {
				return err
			}
			e.Status = outbox.StatusPending
			events = append(events, e)
		}
		return rows.Err()
	})
	return events, err
}

// Ack marks events PUBLISHED in a separate transaction. The row is updated
// only when lease_owner matches and the lease has not expired.
func (r *OutboxRelay) Ack(ctx context.Context, ids ...uuid.UUID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var n int64
	err := doTx(ctx, r.db, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE platform.outbox_events
			   SET status = 'PUBLISHED',
			       published_at = now(),
			       processing_until = NULL,
			       lease_owner = NULL
			 WHERE id = ANY($1)
			   AND lease_owner = $2
			   AND status = 'PENDING'
			   AND processing_until IS NOT NULL
			   AND processing_until >= now()`,
			ids, r.Owner)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}

// Fail records last_error, clears the lease, applies backoff, and marks DEAD
// when attempts have reached MaxAttempts. A stale owner updates 0 rows.
func (r *OutboxRelay) Fail(ctx context.Context, id uuid.UUID, errMsg string) (int64, error) {
	max := r.MaxAttempts
	if max <= 0 {
		max = outbox.DefaultMaxAttempts
	}
	if len(errMsg) > 2000 {
		errMsg = errMsg[:2000]
	}
	var n int64
	err := doTx(ctx, r.db, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE platform.outbox_events
			   SET processing_until = NULL,
			       lease_owner = NULL,
			       last_error = $3,
			       next_attempt_at = now() + CASE
			           WHEN attempts <= 1 THEN interval '1 second'
			           WHEN attempts = 2 THEN interval '5 seconds'
			           WHEN attempts = 3 THEN interval '30 seconds'
			           ELSE interval '5 minutes'
			       END,
			       status = CASE WHEN attempts >= $4 THEN 'DEAD' ELSE 'PENDING' END
			 WHERE id = $1
			   AND lease_owner = $2
			   AND status = 'PENDING'`,
			id, r.Owner, errMsg, max)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}

// PublishBatch publishes claimed events outside a transaction, then ACKs
// each success. On publish error it FAILs that event and skips later events
// of the same aggregate in this batch (concurrency.md §7.2).
func (r *OutboxRelay) PublishBatch(ctx context.Context, pub outbox.Publisher, events []outbox.Event) error {
	if pub == nil {
		return errors.New("outbox publish: publisher is required")
	}
	blocked := map[uuid.UUID]struct{}{}
	var first error
	for _, e := range events {
		if _, skip := blocked[e.AggregateID]; skip {
			continue
		}
		if err := pub.Publish(ctx, e); err != nil {
			if first == nil {
				first = err
			}
			blocked[e.AggregateID] = struct{}{}
			if _, failErr := r.Fail(ctx, e.ID, err.Error()); failErr != nil && first == nil {
				first = failErr
			}
			continue
		}
		n, err := r.Ack(ctx, e.ID)
		if err != nil {
			if first == nil {
				first = err
			}
			blocked[e.AggregateID] = struct{}{}
			continue
		}
		if n == 0 {
			// Lease expired or another owner claimed; at-least-once duplicate is allowed.
			blocked[e.AggregateID] = struct{}{}
		}
	}
	return first
}
