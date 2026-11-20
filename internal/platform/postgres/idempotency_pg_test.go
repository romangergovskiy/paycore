package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/platform/idempotency"
)

func TestHTTPIdempotencyPostgres(t *testing.T) {
	ctx, c := startMigrated(t)
	owner := mustConnect(t, ctx, c.owner)
	defer owner.Close(ctx)
	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)
	createDemoTable(t, ctx, owner)

	user := mustNewID(t)
	scope := idempotency.UserScope(user)
	endpoint := "POST /api/v1/demo"

	t.Run("first request", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "first-1", `{"n":1}`)
		res, err := runDemo(ctx, app, req, demoOp{payload: "one"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Replayed || res.Response.Status != 201 {
			t.Fatalf("%+v", res)
		}
		if countDemo(t, ctx, app, scope, req.Key) != 1 {
			t.Fatal("effect missing")
		}
	})

	t.Run("replay same key and body", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "replay-1", `{"n":1}`)
		first, err := runDemo(ctx, app, req, demoOp{payload: "a"})
		if err != nil {
			t.Fatal(err)
		}
		second, err := runDemo(ctx, app, req, demoOp{payload: "a"})
		if err != nil {
			t.Fatal(err)
		}
		if !second.Replayed {
			t.Fatal("expected replay")
		}
		if first.Response.Status != second.Response.Status {
			t.Fatalf("status %d vs %d", first.Response.Status, second.Response.Status)
		}
		if string(first.Response.Body) != string(second.Response.Body) {
			t.Fatalf("body %s vs %s", first.Response.Body, second.Response.Body)
		}
		if countDemo(t, ctx, app, scope, req.Key) != 1 {
			t.Fatal("second effect")
		}
	})

	t.Run("same key different body", func(t *testing.T) {
		reqA := demoRequest(t, scope, endpoint, "conflict-1", `{"n":1}`)
		if _, err := runDemo(ctx, app, reqA, demoOp{payload: "a"}); err != nil {
			t.Fatal(err)
		}
		reqB := demoRequest(t, scope, endpoint, "conflict-1", `{"n":2}`)
		_, err := runDemo(ctx, app, reqB, demoOp{payload: "b"})
		if !errors.Is(err, idempotency.ErrKeyReused) {
			t.Fatalf("got %v", err)
		}
		if countDemo(t, ctx, app, scope, reqA.Key) != 1 {
			t.Fatal("conflict ran a second effect")
		}
	})

	t.Run("parallel identical requests", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "parallel-same", `{"n":1}`)
		type outcome struct {
			res idempotency.Result
			err error
		}
		ch := make(chan outcome, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn := mustConnect(t, ctx, c.app)
				defer conn.Close(ctx)
				<-start
				res, err := runDemo(ctx, conn, req, demoOp{payload: "p"})
				ch <- outcome{res, err}
			}()
		}
		close(start)
		wg.Wait()
		close(ch)
		var ok, replayed, failed int
		for o := range ch {
			if o.err != nil {
				failed++
				t.Errorf("parallel err: %v", o.err)
				continue
			}
			ok++
			if o.res.Replayed {
				replayed++
			}
		}
		if failed > 0 || ok != 2 {
			t.Fatalf("ok=%d failed=%d (must not both fail)", ok, failed)
		}
		if replayed != 1 {
			t.Fatalf("replayed=%d want 1 (one effect, one replay)", replayed)
		}
		if countDemo(t, ctx, app, scope, req.Key) != 1 {
			t.Fatal("parallel created two rows")
		}
	})

	t.Run("concurrent different body", func(t *testing.T) {
		key := "parallel-diff"
		reqA := demoRequest(t, scope, endpoint, key, `{"n":1}`)
		reqB := demoRequest(t, scope, endpoint, key, `{"n":2}`)
		type outcome struct {
			res idempotency.Result
			err error
		}
		ch := make(chan outcome, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		run := func(req idempotency.Request) {
			defer wg.Done()
			conn := mustConnect(t, ctx, c.app)
			defer conn.Close(ctx)
			<-start
			res, err := runDemo(ctx, conn, req, demoOp{payload: "x"})
			ch <- outcome{res, err}
		}
		wg.Add(2)
		go run(reqA)
		go run(reqB)
		close(start)
		wg.Wait()
		close(ch)
		var reused, executed, failed int
		for o := range ch {
			if o.err != nil {
				if errors.Is(o.err, idempotency.ErrKeyReused) {
					reused++
					continue
				}
				failed++
				t.Errorf("err %v", o.err)
				continue
			}
			if o.res.Replayed {
				t.Error("replay on different body")
			} else {
				executed++
			}
		}
		if failed > 0 || executed != 1 || reused != 1 {
			t.Fatalf("executed=%d reused=%d failed=%d", executed, reused, failed)
		}
		if countDemo(t, ctx, app, scope, key) != 1 {
			t.Fatal("different-body concurrent created two effects")
		}
	})

	t.Run("rollback before commit", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "rollback-1", `{"n":1}`)
		_, err := runDemo(ctx, app, req, demoOp{fail: errors.New("boom")})
		if err == nil {
			t.Fatal("expected error")
		}
		if countDemo(t, ctx, app, scope, req.Key) != 0 {
			t.Fatal("effect survived rollback")
		}
		if countKeys(t, ctx, app, scope, endpoint, req.Key) != 0 {
			t.Fatal("key survived rollback")
		}
		res, err := runDemo(ctx, app, req, demoOp{payload: "after"})
		if err != nil || res.Replayed {
			t.Fatalf("%+v %v", res, err)
		}
		if countDemo(t, ctx, app, scope, req.Key) != 1 {
			t.Fatal("retry after rollback did not run")
		}
	})

	t.Run("replay restores full response", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "replay-full", `{"n":1}`)
		first, err := runDemo(ctx, app, req, demoOp{payload: "full"})
		if err != nil {
			t.Fatal(err)
		}
		if first.Response.ResourceType != "demo" || first.Response.ResourceID == nil {
			t.Fatalf("first missing resource fields: %+v", first.Response)
		}
		second, err := runDemo(ctx, app, req, demoOp{payload: "full"})
		if err != nil || !second.Replayed {
			t.Fatalf("%+v %v", second, err)
		}
		if first.Response.Status != second.Response.Status ||
			string(first.Response.Body) != string(second.Response.Body) ||
			first.Response.ResourceType != second.Response.ResourceType ||
			second.Response.ResourceID == nil ||
			*first.Response.ResourceID != *second.Response.ResourceID {
			t.Fatalf("replay mismatch first=%+v second=%+v", first.Response, second.Response)
		}
		if countDemo(t, ctx, app, scope, req.Key) != 1 {
			t.Fatal("full replay created a second effect")
		}
	})

	t.Run("replay without resource fields", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "replay-null-res", `{"n":1}`)
		body := json.RawMessage(`{"ok":true}`)
		var first idempotency.Result
		err := RetryTx(ctx, app, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			first, err = ExecuteIdempotent(ctx, tx, req, func(context.Context) (idempotency.Response, error) {
				return idempotency.Response{Status: 201, Body: body}, nil
			})
			return err
		})
		if err != nil || first.Replayed {
			t.Fatalf("%+v %v", first, err)
		}
		var second idempotency.Result
		err = RetryTx(ctx, app, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			second, err = ExecuteIdempotent(ctx, tx, req, func(context.Context) (idempotency.Response, error) {
				t.Error("op ran on replay")
				return idempotency.Response{}, errors.New("must not run")
			})
			return err
		})
		if err != nil || !second.Replayed {
			t.Fatalf("%+v %v", second, err)
		}
		if second.Response.Status != 201 || string(second.Response.Body) != string(body) {
			t.Fatalf("replay body/status %+v", second.Response)
		}
		if second.Response.ResourceType != "" || second.Response.ResourceID != nil {
			t.Fatalf("NULL resource fields must stay absent: %+v", second.Response)
		}
	})

	t.Run("lost HTTP after commit then replay", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "lost-http", `{"n":1}`)
		first, err := runDemo(ctx, app, req, demoOp{payload: "ok"})
		if err != nil {
			t.Fatal(err)
		}
		second, err := runDemo(ctx, app, req, demoOp{payload: "ok"})
		if err != nil || !second.Replayed {
			t.Fatalf("%+v %v", second, err)
		}
		if string(first.Response.Body) != string(second.Response.Body) {
			t.Fatal("replay body mismatch")
		}
		if countDemo(t, ctx, app, scope, req.Key) != 1 {
			t.Fatal("lost-http double effect")
		}
	})

	t.Run("persist 422", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "biz-422", `{"n":1}`)
		first, err := runDemo(ctx, app, req, demoOp{payload: "failed", status: 422})
		if err != nil {
			t.Fatal(err)
		}
		if first.Response.Status != 422 {
			t.Fatalf("status %d", first.Response.Status)
		}
		second, err := runDemo(ctx, app, req, demoOp{payload: "failed", status: 201})
		if err != nil || !second.Replayed || second.Response.Status != 422 {
			t.Fatalf("%+v %v", second, err)
		}
		if countDemo(t, ctx, app, scope, req.Key) != 1 {
			t.Fatal("422 replay created another row")
		}
	})

	t.Run("do not persist 5xx", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "err-5xx", `{"n":1}`)
		err := RetryTx(ctx, app, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ExecuteIdempotent(ctx, tx, req, func(context.Context) (idempotency.Response, error) {
				return idempotency.Response{Status: 500, Body: json.RawMessage(`{}`)}, nil
			})
			return err
		})
		if err == nil {
			t.Fatal("expected refuse 5xx")
		}
		if countKeys(t, ctx, app, scope, endpoint, req.Key) != 0 {
			t.Fatal("5xx persisted a completed key")
		}
		res, err := runDemo(ctx, app, req, demoOp{payload: "ok"})
		if err != nil || res.Replayed || res.Response.Status != 201 {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("ttl cleanup then business unique", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "ttl-1", `{"n":1}`)
		if _, err := runDemo(ctx, app, req, demoOp{payload: "live"}); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Exec(ctx, `
			UPDATE platform.idempotency_keys
			   SET expires_at = now() - interval '1 second'
			 WHERE scope = $1 AND endpoint = $2 AND key = $3`,
			scope, endpoint, req.Key); err != nil {
			t.Fatal(err)
		}
		n, err := CleanupExpiredIdempotencyKeys(ctx, app, 100)
		if err != nil || n < 1 {
			t.Fatalf("cleanup %d %v", n, err)
		}
		if countKeys(t, ctx, app, scope, endpoint, req.Key) != 0 {
			t.Fatal("expired key remained")
		}
		_, err = runDemo(ctx, app, req, demoOp{payload: "again"})
		if !errors.Is(err, idempotency.ErrKeyReused) {
			t.Fatalf("after ttl got %v", err)
		}
		if countDemo(t, ctx, app, scope, req.Key) != 1 {
			t.Fatal("ttl cleanup allowed a second business effect")
		}
	})

	t.Run("in progress not visible committed", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "inprog-1", `{"n":1}`)
		holding := make(chan struct{})
		release := make(chan struct{})
		errCh := make(chan error, 1)
		go func() {
			conn := mustConnect(t, ctx, c.app)
			defer conn.Close(ctx)
			tx, err := conn.Begin(ctx)
			if err != nil {
				errCh <- err
				close(holding)
				return
			}
			_, err = ExecuteIdempotent(ctx, tx, req, func(context.Context) (idempotency.Response, error) {
				close(holding)
				<-release
				id, err := insertDemo(ctx, tx, req.Scope, req.Key, "hold")
				if err != nil {
					return idempotency.Response{}, err
				}
				body, _ := json.Marshal(map[string]string{"id": id.String()})
				return idempotency.Response{Status: 201, Body: body, ResourceID: &id}, nil
			})
			if err != nil {
				_ = tx.Rollback(ctx)
				errCh <- err
				return
			}
			errCh <- tx.Commit(ctx)
		}()
		<-holding
		other := mustConnect(t, ctx, c.app)
		defer other.Close(ctx)
		if n := countKeys(t, ctx, other, scope, endpoint, req.Key); n != 0 {
			close(release)
			t.Fatalf("other session saw uncommitted IN_PROGRESS n=%d", n)
		}
		close(release)
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("lock timeout request in progress", func(t *testing.T) {
		req := demoRequest(t, scope, endpoint, "lockto-1", `{"n":1}`)
		holding := make(chan struct{})
		release := make(chan struct{})
		errA := make(chan error, 1)
		go func() {
			conn := mustConnect(t, ctx, c.app)
			defer conn.Close(ctx)
			tx, err := conn.Begin(ctx)
			if err != nil {
				errA <- err
				close(holding)
				return
			}
			_, err = ExecuteIdempotent(ctx, tx, req, func(context.Context) (idempotency.Response, error) {
				close(holding)
				<-release
				id, err := insertDemo(ctx, tx, req.Scope, req.Key, "slow")
				if err != nil {
					return idempotency.Response{}, err
				}
				return idempotency.Response{Status: 201, Body: json.RawMessage(`{"ok":true}`), ResourceID: &id}, nil
			})
			if err != nil {
				_ = tx.Rollback(ctx)
				errA <- err
				return
			}
			errA <- tx.Commit(ctx)
		}()
		<-holding
		b := mustConnect(t, ctx, c.app)
		defer b.Close(ctx)
		if _, err := b.Exec(ctx, `SET lock_timeout = '200ms'`); err != nil {
			close(release)
			t.Fatal(err)
		}
		_, err := runDemo(ctx, b, req, demoOp{payload: "b"})
		close(release)
		if aerr := <-errA; aerr != nil {
			t.Fatalf("holder: %v", aerr)
		}
		if !errors.Is(err, idempotency.ErrInProgress) {
			t.Fatalf("want REQUEST_IN_PROGRESS, got %v", err)
		}
		if countDemo(t, ctx, app, scope, req.Key) != 1 {
			t.Fatal("lock-timeout path created extra effect")
		}
	})

	t.Run("merchant scope not api key", func(t *testing.T) {
		mid := mustNewID(t)
		req := demoRequest(t, idempotency.MerchantScope(mid), endpoint, "m-1", `{"n":1}`)
		if _, err := runDemo(ctx, app, req, demoOp{payload: "m"}); err != nil {
			t.Fatal(err)
		}
		bad := req
		bad.Scope = "apikey:" + mustNewID(t).String()
		if _, err := runDemo(ctx, app, bad, demoOp{payload: "m"}); err == nil {
			t.Fatal("API key scope accepted")
		}
	})

	t.Run("retry honors cancel", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		err := RetryTx(cctx, app, func(context.Context, pgx.Tx) error {
			t.Fatal("fn ran")
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}
