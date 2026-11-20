package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/platform/idempotency"
)

// ExecuteIdempotent runs D-07 in the caller's transaction:
// INSERT IN_PROGRESS → op (business effect + optional outbox) → UPDATE COMPLETED.
// The same key with the same body returns the saved response. A different body
// returns idempotency.ErrKeyReused without running op. 5xx is not stored:
// op must return an error so this transaction rolls back.
func ExecuteIdempotent(
	ctx context.Context,
	tx pgx.Tx,
	req idempotency.Request,
	op func(context.Context) (idempotency.Response, error),
) (idempotency.Result, error) {
	if op == nil {
		return idempotency.Result{}, errors.New("idempotency: operation is required")
	}
	if req.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return idempotency.Result{}, err
		}
		req.ID = id
	}
	if err := req.Validate(); err != nil {
		return idempotency.Result{}, err
	}

	var inserted uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO platform.idempotency_keys
		    (id, scope, endpoint, key, request_hash, status, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, 'IN_PROGRESS', now(), now() + interval '24 hours')
		ON CONFLICT (scope, endpoint, key) DO NOTHING
		RETURNING id`,
		req.ID, req.Scope, req.Endpoint, req.Key, req.Hash,
	).Scan(&inserted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return idempotency.Result{}, fmt.Errorf("idempotency begin: %w", err)
	}
	if err == nil {
		resp, opErr := op(ctx)
		if opErr != nil {
			return idempotency.Result{}, opErr
		}
		if !idempotency.Persistable(resp.Status) {
			return idempotency.Result{}, fmt.Errorf("idempotency: refusing to persist HTTP %d", resp.Status)
		}
		if err := completeIdempotency(ctx, tx, inserted, resp); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Response: resp}, nil
	}

	var (
		hash   []byte
		status string
		code   *int
		body   []byte
	)
	err = tx.QueryRow(ctx, `
		SELECT request_hash, status, response_code, response_body
		  FROM platform.idempotency_keys
		 WHERE scope = $1 AND endpoint = $2 AND key = $3`,
		req.Scope, req.Endpoint, req.Key,
	).Scan(&hash, &status, &code, &body)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("idempotency lookup: %w", err)
	}
	if !bytes.Equal(hash, req.Hash) {
		return idempotency.Result{}, idempotency.ErrKeyReused
	}
	if status == idempotency.StatusCompleted {
		resp := idempotency.Response{Body: compactJSON(body)}
		if code != nil {
			resp.Status = *code
		}
		return idempotency.Result{Response: resp, Replayed: true}, nil
	}
	return idempotency.Result{}, idempotency.ErrInProgress
}

func completeIdempotency(ctx context.Context, tx pgx.Tx, id uuid.UUID, resp idempotency.Response) error {
	tag, err := tx.Exec(ctx, `
		UPDATE platform.idempotency_keys
		   SET status = 'COMPLETED',
		       response_code = $2,
		       response_body = $3::jsonb,
		       resource_type = $4,
		       resource_id = $5,
		       completed_at = now()
		 WHERE id = $1 AND status = 'IN_PROGRESS'`,
		id, resp.Status, jsonText(resp.Body), nullIfEmpty(resp.ResourceType), resp.ResourceID,
	)
	if err != nil {
		return fmt.Errorf("idempotency complete: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("idempotency complete: key was not IN_PROGRESS")
	}
	return nil
}

func jsonText(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func compactJSON(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return b
	}
	return buf.Bytes()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CleanupExpiredIdempotencyKeys deletes expired keys in a small batch.
// This is SQL for tests and later workers, not a daemon in Stage A.
func CleanupExpiredIdempotencyKeys(ctx context.Context, db beginner, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	var n int64
	err := doTx(ctx, db, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM platform.idempotency_keys
			 WHERE id IN (
			     SELECT id FROM platform.idempotency_keys
			      WHERE expires_at < now()
			      LIMIT $1
			 )`, limit)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}
