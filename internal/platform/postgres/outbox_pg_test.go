package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/platform/idempotency"
	"github.com/romangergovskiy/paycore/internal/platform/outbox"
)

func TestOutboxPostgres(t *testing.T) {
	ctx, c := startMigrated(t)
	owner := mustConnect(t, ctx, c.owner)
	defer owner.Close(ctx)
	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)
	createDemoTable(t, ctx, owner)

	t.Run("insert in same tx as effect", func(t *testing.T) {
		scope := idempotency.UserScope(mustNewID(t))
		req := demoRequest(t, scope, "POST /api/v1/demo", "obx-1", `{"n":1}`)
		if _, err := runDemo(ctx, app, req, demoOp{payload: "e", withOutbox: true}); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events WHERE aggregate_type = 'demo'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("outbox rows = %d", n)
		}
		if countDemo(t, ctx, app, scope, req.Key) != 1 {
			t.Fatal("effect missing")
		}
	})

	t.Run("rollback removes effect and outbox", func(t *testing.T) {
		agg := mustNewID(t)
		err := RetryTx(ctx, app, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := insertDemo(ctx, tx, "user:"+agg.String(), "rb", "x"); err != nil {
				return err
			}
			if _, err := InsertOutboxEvent(ctx, tx, outbox.Event{
				AggregateType: "demo-rb", AggregateID: agg, AggregateVersion: 1,
				Type: "demo.created", Version: 1, Payload: json.RawMessage(`{}`),
			}); err != nil {
				return err
			}
			return errors.New("boom")
		})
		if err == nil {
			t.Fatal("expected rollback")
		}
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events WHERE aggregate_id = $1`, agg).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("outbox survived rollback")
		}
		if countDemo(t, ctx, app, "user:"+agg.String(), "rb") != 0 {
			t.Fatal("demo survived rollback")
		}
	})

	t.Run("seq order within aggregate", func(t *testing.T) {
		drainOutbox(t, ctx, app)
		agg := mustNewID(t)
		e1, e2 := insertPair(t, ctx, app, agg)
		relay := NewOutboxRelay(app, "relay-order")
		batch, err := relay.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) != 1 || batch[0].ID != e1.ID {
			t.Fatalf("claimed %+v want only seq1 %s", ids(batch), e1.ID)
		}
		n, err := relay.Ack(ctx, batch[0].ID)
		if err != nil || n != 1 {
			t.Fatalf("ack %d %v", n, err)
		}
		batch, err = relay.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) != 1 || batch[0].ID != e2.ID {
			t.Fatalf("second claim %+v want %s", ids(batch), e2.ID)
		}
	})

	t.Run("later cannot bypass earlier pending", func(t *testing.T) {
		drainOutbox(t, ctx, app)
		agg := mustNewID(t)
		e1, _ := insertPair(t, ctx, app, agg)
		relay := NewOutboxRelay(app, "relay-bypass")
		batch, err := relay.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) != 1 || batch[0].ID != e1.ID {
			t.Fatalf("got %+v", ids(batch))
		}
		again, err := relay.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != 0 {
			t.Fatalf("later event bypassed earlier lease: %+v", ids(again))
		}
	})

	t.Run("independent aggregates", func(t *testing.T) {
		drainOutbox(t, ctx, app)
		a, b := mustNewID(t), mustNewID(t)
		ea := insertOne(t, ctx, app, a, "demo.a")
		eb := insertOne(t, ctx, app, b, "demo.b")
		relay := NewOutboxRelay(app, "relay-ind")
		relay.BatchSize = 10
		batch, err := relay.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := map[uuid.UUID]bool{}
		for _, e := range batch {
			got[e.ID] = true
		}
		if !got[ea.ID] || !got[eb.ID] {
			t.Fatalf("independent aggregates not claimed together: %+v", ids(batch))
		}
	})

	t.Run("two concurrent relays", func(t *testing.T) {
		drainOutbox(t, ctx, app)
		var created []uuid.UUID
		for i := 0; i < 6; i++ {
			e := insertOne(t, ctx, app, mustNewID(t), "demo.conc")
			created = append(created, e.ID)
		}
		start := make(chan struct{})
		type got struct {
			ids []uuid.UUID
			err error
		}
		ch := make(chan got, 2)
		var wg sync.WaitGroup
		for _, ownerName := range []string{"relay-a", "relay-b"} {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				conn := mustConnect(t, ctx, c.app)
				defer conn.Close(ctx)
				r := NewOutboxRelay(conn, name)
				r.BatchSize = 3
				<-start
				batch, err := r.Claim(ctx)
				var out []uuid.UUID
				for _, e := range batch {
					out = append(out, e.ID)
				}
				ch <- got{out, err}
			}(ownerName)
		}
		close(start)
		wg.Wait()
		close(ch)
		seen := map[uuid.UUID]int{}
		var failed int
		var total int
		for g := range ch {
			if g.err != nil {
				failed++
				t.Errorf("claim: %v", g.err)
				continue
			}
			if len(g.ids) == 0 {
				t.Error("one claimer got an empty batch")
			}
			total += len(g.ids)
			for _, id := range g.ids {
				seen[id]++
			}
		}
		if failed > 0 {
			t.Fatal("concurrent claim failed")
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("event %s claimed %d times", id, n)
			}
		}
		if total != 6 {
			t.Fatalf("claimed %d want 6", total)
		}
		for _, id := range created {
			if seen[id] != 1 {
				t.Fatalf("created event %s claimed %d times", id, seen[id])
			}
		}
	})

	t.Run("lease expiry re-claim and stale ack", func(t *testing.T) {
		drainOutbox(t, ctx, app)
		e := insertOne(t, ctx, app, mustNewID(t), "demo.lease")
		a := NewOutboxRelay(app, "owner-a")
		batch, err := a.Claim(ctx)
		if err != nil || len(batch) != 1 || batch[0].ID != e.ID {
			t.Fatalf("claim A %+v %v", ids(batch), err)
		}
		if _, err := app.Exec(ctx, `
			UPDATE platform.outbox_events
			   SET processing_until = now() - interval '1 second'
			 WHERE id = $1`, e.ID); err != nil {
			t.Fatal(err)
		}
		bConn := mustConnect(t, ctx, c.app)
		defer bConn.Close(ctx)
		b := NewOutboxRelay(bConn, "owner-b")
		reclaimed, err := b.Claim(ctx)
		if err != nil || len(reclaimed) != 1 || reclaimed[0].ID != e.ID {
			t.Fatalf("reclaim %+v %v", ids(reclaimed), err)
		}
		n, err := a.Ack(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("stale owner ACKed")
		}
		n, err = b.Ack(ctx, e.ID)
		if err != nil || n != 1 {
			t.Fatalf("live owner ack %d %v", n, err)
		}
	})

	t.Run("retry backoff", func(t *testing.T) {
		drainOutbox(t, ctx, app)
		e := insertOne(t, ctx, app, mustNewID(t), "demo.bo")
		r := NewOutboxRelay(app, "owner-bo")
		r.MaxAttempts = 8
		if _, err := r.Claim(ctx); err != nil {
			t.Fatal(err)
		}
		n, err := r.Fail(ctx, e.ID, "publisher down")
		if err != nil || n != 1 {
			t.Fatalf("fail %d %v", n, err)
		}
		batch, err := r.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range batch {
			if ev.ID == e.ID {
				t.Fatal("claimed event still in backoff")
			}
		}
		if _, err := app.Exec(ctx, `
			UPDATE platform.outbox_events SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, e.ID); err != nil {
			t.Fatal(err)
		}
		batch, err = r.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ev := range batch {
			if ev.ID == e.ID {
				found = true
			}
		}
		if !found {
			t.Fatal("event not reclaimed after backoff elapsed")
		}
	})

	t.Run("dead does not skip later seq", func(t *testing.T) {
		drainOutbox(t, ctx, app)
		agg := mustNewID(t)
		e1, e2 := insertPair(t, ctx, app, agg)
		r := NewOutboxRelay(app, "owner-dead")
		r.MaxAttempts = 1
		batch, err := r.Claim(ctx)
		if err != nil || len(batch) != 1 || batch[0].ID != e1.ID {
			t.Fatalf("claim %+v %v", ids(batch), err)
		}
		n, err := r.Fail(ctx, e1.ID, "poison")
		if err != nil || n != 1 {
			t.Fatalf("fail %d %v", n, err)
		}
		var status string
		if err := app.QueryRow(ctx, `SELECT status FROM platform.outbox_events WHERE id = $1`, e1.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != outbox.StatusDead {
			t.Fatalf("status %s", status)
		}
		later, err := r.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range later {
			if ev.ID == e2.ID {
				t.Fatal("DEAD earlier seq silently released later event")
			}
		}
	})

	t.Run("stale owner cannot fail", func(t *testing.T) {
		drainOutbox(t, ctx, app)
		e := insertOne(t, ctx, app, mustNewID(t), "demo.stale-fail")
		a := NewOutboxRelay(app, "fail-a")
		if _, err := a.Claim(ctx); err != nil {
			t.Fatal(err)
		}
		b := NewOutboxRelay(app, "fail-b")
		n, err := b.Fail(ctx, e.ID, "not mine")
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("stale owner failed the row")
		}
	})

	t.Run("claim empty", func(t *testing.T) {
		drainOutbox(t, ctx, app)
		r := NewOutboxRelay(app, "empty")
		batch, err := r.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) != 0 {
			t.Fatalf("empty claimed %+v", ids(batch))
		}
	})

	t.Run("publish batch then ack", func(t *testing.T) {
		drainOutbox(t, ctx, app)
		e := insertOne(t, ctx, app, mustNewID(t), "demo.pub")
		r := NewOutboxRelay(app, "owner-pub")
		batch, err := r.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pub := &recordPublisher{}
		if err := r.PublishBatch(ctx, pub, batch); err != nil {
			t.Fatal(err)
		}
		if len(pub.ids) != 1 || pub.ids[0] != e.ID {
			t.Fatalf("published %+v", pub.ids)
		}
		var status string
		if err := app.QueryRow(ctx, `SELECT status FROM platform.outbox_events WHERE id = $1`, e.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != outbox.StatusPublished {
			t.Fatalf("status %s", status)
		}
	})
}

type recordPublisher struct {
	ids []uuid.UUID
	err error
}

func (p *recordPublisher) Publish(ctx context.Context, e outbox.Event) error {
	p.ids = append(p.ids, e.ID)
	return p.err
}

func insertOne(t *testing.T, ctx context.Context, db beginner, agg uuid.UUID, typ string) outbox.Event {
	t.Helper()
	var e outbox.Event
	err := RetryTx(ctx, db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e, err = InsertOutboxEvent(ctx, tx, outbox.Event{
			AggregateType: "demo", AggregateID: agg, AggregateVersion: 1,
			Type: typ, Version: 1, Payload: json.RawMessage(`{"ok":true}`),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func insertPair(t *testing.T, ctx context.Context, db beginner, agg uuid.UUID) (outbox.Event, outbox.Event) {
	t.Helper()
	var a, b outbox.Event
	err := RetryTx(ctx, db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = InsertOutboxEvent(ctx, tx, outbox.Event{
			AggregateType: "demo", AggregateID: agg, AggregateVersion: 1,
			Type: "demo.first", Version: 1, Payload: json.RawMessage(`{"n":1}`),
		})
		if err != nil {
			return err
		}
		b, err = InsertOutboxEvent(ctx, tx, outbox.Event{
			AggregateType: "demo", AggregateID: agg, AggregateVersion: 2,
			Type: "demo.second", Version: 1, Payload: json.RawMessage(`{"n":2}`),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Seq >= b.Seq {
		t.Fatalf("seq %d then %d", a.Seq, b.Seq)
	}
	return a, b
}

func ids(ev []outbox.Event) []uuid.UUID {
	out := make([]uuid.UUID, len(ev))
	for i, e := range ev {
		out[i] = e.ID
	}
	return out
}

func drainOutbox(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	if _, err := conn.Exec(ctx, `
		UPDATE platform.outbox_events
		   SET status = 'PUBLISHED',
		       published_at = COALESCE(published_at, now()),
		       processing_until = NULL,
		       lease_owner = NULL
		 WHERE status = 'PENDING'`); err != nil {
		t.Fatal(err)
	}
}
