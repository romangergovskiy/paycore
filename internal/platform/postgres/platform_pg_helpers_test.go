package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/platform/idempotency"
	"github.com/romangergovskiy/paycore/internal/platform/outbox"
)

func startMigrated(t *testing.T) (context.Context, testCluster) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	c := startCluster(t)
	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}
	return ctx, c
}

func createDemoTable(t *testing.T, ctx context.Context, owner *pgx.Conn) {
	t.Helper()
	if _, err := owner.Exec(ctx, `
		CREATE TABLE platform.stage_a_demo_resources (
		    id            UUID PRIMARY KEY,
		    owner_scope   TEXT NOT NULL,
		    business_key  TEXT NOT NULL,
		    payload       TEXT NOT NULL,
		    CONSTRAINT uq_stage_a_demo_resources UNIQUE (owner_scope, business_key)
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `
		GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE platform.stage_a_demo_resources TO paycore_app`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `
		CREATE TABLE platform.stage_a_inbox_effects (
		    consumer    TEXT NOT NULL,
		    message_id  UUID NOT NULL,
		    n           INT NOT NULL,
		    PRIMARY KEY (consumer, message_id)
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `
		GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE platform.stage_a_inbox_effects TO paycore_app`); err != nil {
		t.Fatal(err)
	}
}

func demoRequest(t *testing.T, scope, endpoint, key, body string) idempotency.Request {
	t.Helper()
	canon, err := idempotency.CanonicalJSON([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return idempotency.Request{
		Scope:    scope,
		Endpoint: endpoint,
		Key:      key,
		Hash:     idempotency.Hash(endpoint, nil, canon),
	}
}

func insertDemo(ctx context.Context, tx pgx.Tx, scope, key, payload string) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO platform.stage_a_demo_resources (id, owner_scope, business_key, payload)
		VALUES ($1, $2, $3, $4)`, id, scope, key, payload)
	if isUniqueViolation(err) {
		return uuid.Nil, idempotency.ErrKeyReused
	}
	return id, err
}

func countDemo(t *testing.T, ctx context.Context, conn *pgx.Conn, scope, key string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM platform.stage_a_demo_resources
		 WHERE owner_scope = $1 AND business_key = $2`, scope, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countKeys(t *testing.T, ctx context.Context, conn *pgx.Conn, scope, endpoint, key string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM platform.idempotency_keys
		 WHERE scope = $1 AND endpoint = $2 AND key = $3`, scope, endpoint, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type demoOp struct {
	payload    string
	status     int
	fail       error
	withOutbox bool
	aggregate  uuid.UUID
}

func runDemo(ctx context.Context, db beginner, req idempotency.Request, op demoOp) (idempotency.Result, error) {
	if op.status == 0 {
		op.status = 201
	}
	var res idempotency.Result
	err := RetryTx(ctx, db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = ExecuteIdempotent(ctx, tx, req, func(ctx context.Context) (idempotency.Response, error) {
			if op.fail != nil {
				return idempotency.Response{}, op.fail
			}
			id, err := insertDemo(ctx, tx, req.Scope, req.Key, op.payload)
			if err != nil {
				return idempotency.Response{}, err
			}
			if op.withOutbox {
				agg := op.aggregate
				if agg == uuid.Nil {
					agg = id
				}
				if _, err := InsertOutboxEvent(ctx, tx, outbox.Event{
					AggregateType:    "demo",
					AggregateID:      agg,
					AggregateVersion: 1,
					Type:             "demo.created",
					Version:          1,
					Payload:          json.RawMessage(`{"ok":true}`),
				}); err != nil {
					return idempotency.Response{}, err
				}
			}
			body, err := json.Marshal(map[string]string{"id": id.String()})
			if err != nil {
				return idempotency.Response{}, err
			}
			return idempotency.Response{
				Status:       op.status,
				Body:         body,
				ResourceType: "demo",
				ResourceID:   &id,
			}, nil
		})
		return err
	})
	return res, err
}
