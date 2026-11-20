package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ProcessInbox inserts (consumer, message_id). On a new row it runs effect
// and sets processed_at in the same transaction. A repeat delivery sees the
// unique conflict, does not run effect, and returns already=true.
// If effect returns an error, the caller must roll back so the row is not
// left processed.
func ProcessInbox(
	ctx context.Context,
	tx pgx.Tx,
	consumer string,
	messageID uuid.UUID,
	effect func(context.Context) error,
) (already bool, err error) {
	if consumer == "" {
		return false, errors.New("inbox: consumer is required")
	}
	if messageID == uuid.Nil {
		return false, errors.New("inbox: message id is required")
	}
	if effect == nil {
		return false, errors.New("inbox: effect is required")
	}
	var inserted bool
	err = tx.QueryRow(ctx, `
		INSERT INTO platform.inbox_messages (consumer, message_id, received_at)
		VALUES ($1, $2, now())
		ON CONFLICT (consumer, message_id) DO NOTHING
		RETURNING true`,
		consumer, messageID,
	).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inbox begin: %w", err)
	}
	if err := effect(ctx); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE platform.inbox_messages
		   SET processed_at = now()
		 WHERE consumer = $1 AND message_id = $2 AND processed_at IS NULL`,
		consumer, messageID)
	if err != nil {
		return false, fmt.Errorf("inbox complete: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("inbox complete: row was not pending")
	}
	return false, nil
}
