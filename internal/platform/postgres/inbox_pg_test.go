package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestInboxPostgres(t *testing.T) {
	ctx, c := startMigrated(t)
	owner := mustConnect(t, ctx, c.owner)
	defer owner.Close(ctx)
	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)
	createDemoTable(t, ctx, owner)

	consumer := "demo-consumer"

	t.Run("first delivery", func(t *testing.T) {
		msg := mustNewID(t)
		err := RetryTx(ctx, app, func(ctx context.Context, tx pgx.Tx) error {
			already, err := ProcessInbox(ctx, tx, consumer, msg, func(ctx context.Context) error {
				return bumpEffect(ctx, tx, consumer, msg)
			})
			if err != nil {
				return err
			}
			if already {
				return errors.New("first delivery marked already")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if n := effectCount(t, ctx, app, consumer, msg); n != 1 {
			t.Fatalf("n=%d", n)
		}
	})

	t.Run("repeat delivery", func(t *testing.T) {
		msg := mustNewID(t)
		run := func() error {
			return RetryTx(ctx, app, func(ctx context.Context, tx pgx.Tx) error {
				_, err := ProcessInbox(ctx, tx, consumer, msg, func(ctx context.Context) error {
					return bumpEffect(ctx, tx, consumer, msg)
				})
				return err
			})
		}
		if err := run(); err != nil {
			t.Fatal(err)
		}
		if err := run(); err != nil {
			t.Fatal(err)
		}
		if n := effectCount(t, ctx, app, consumer, msg); n != 1 {
			t.Fatalf("repeat ran effect n=%d", n)
		}
		var processed bool
		if err := app.QueryRow(ctx, `
			SELECT processed_at IS NOT NULL FROM platform.inbox_messages
			 WHERE consumer = $1 AND message_id = $2`, consumer, msg).Scan(&processed); err != nil {
			t.Fatal(err)
		}
		if !processed {
			t.Fatal("processed_at not set")
		}
	})

	t.Run("parallel delivery", func(t *testing.T) {
		msg := mustNewID(t)
		start := make(chan struct{})
		errCh := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn := mustConnect(t, ctx, c.app)
				defer conn.Close(ctx)
				<-start
				errCh <- RetryTx(ctx, conn, func(ctx context.Context, tx pgx.Tx) error {
					_, err := ProcessInbox(ctx, tx, consumer, msg, func(ctx context.Context) error {
						return bumpEffect(ctx, tx, consumer, msg)
					})
					return err
				})
			}()
		}
		close(start)
		wg.Wait()
		close(errCh)
		var failed int
		for err := range errCh {
			if err != nil {
				failed++
				t.Errorf("parallel inbox: %v", err)
			}
		}
		if failed > 0 {
			t.Fatal("parallel inbox both-or-one failed")
		}
		if n := effectCount(t, ctx, app, consumer, msg); n != 1 {
			t.Fatalf("parallel effect n=%d", n)
		}
	})

	t.Run("processing rollback", func(t *testing.T) {
		msg := mustNewID(t)
		err := RetryTx(ctx, app, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ProcessInbox(ctx, tx, consumer, msg, func(ctx context.Context) error {
				if err := bumpEffect(ctx, tx, consumer, msg); err != nil {
					return err
				}
				return errors.New("handler failed")
			})
			return err
		})
		if err == nil {
			t.Fatal("expected handler error")
		}
		var n int
		if err := app.QueryRow(ctx, `
			SELECT count(*) FROM platform.inbox_messages
			 WHERE consumer = $1 AND message_id = $2`, consumer, msg).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("inbox row survived rollback")
		}
		if effectCount(t, ctx, app, consumer, msg) != 0 {
			t.Fatal("effect survived rollback")
		}
		if err := RetryTx(ctx, app, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ProcessInbox(ctx, tx, consumer, msg, func(ctx context.Context) error {
				return bumpEffect(ctx, tx, consumer, msg)
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if effectCount(t, ctx, app, consumer, msg) != 1 {
			t.Fatal("retry after rollback did not run")
		}
	})

	t.Run("consumer error does not leave processed", func(t *testing.T) {
		msg := mustNewID(t)
		conn := mustConnect(t, ctx, c.app)
		defer conn.Close(ctx)
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ProcessInbox(ctx, tx, consumer, msg, func(ctx context.Context) error {
			return errors.New("no effect")
		})
		if err == nil {
			_ = tx.Rollback(ctx)
			t.Fatal("expected error")
		}
		if rb := tx.Rollback(ctx); rb != nil {
			t.Fatal(rb)
		}
		var processed *int
		err = app.QueryRow(ctx, `
			SELECT 1 FROM platform.inbox_messages
			 WHERE consumer = $1 AND message_id = $2 AND processed_at IS NOT NULL`,
			consumer, msg).Scan(&processed)
		if err == nil {
			t.Fatal("processed_at set after failed handler")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("lookup: %v", err)
		}
	})
}

func bumpEffect(ctx context.Context, tx pgx.Tx, consumer string, msg uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO platform.stage_a_inbox_effects (consumer, message_id, n)
		VALUES ($1, $2, 1)
		ON CONFLICT (consumer, message_id) DO UPDATE
		  SET n = platform.stage_a_inbox_effects.n + 1`, consumer, msg)
	return err
}

func effectCount(t *testing.T, ctx context.Context, conn *pgx.Conn, consumer string, msg uuid.UUID) int {
	t.Helper()
	var n int
	err := conn.QueryRow(ctx, `
		SELECT COALESCE((SELECT n FROM platform.stage_a_inbox_effects
		                  WHERE consumer = $1 AND message_id = $2), 0)`,
		consumer, msg).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
