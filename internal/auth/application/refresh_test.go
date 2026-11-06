package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func loginOnce(t *testing.T, w *world) Credentials {
	t.Helper()
	svc := w.service()
	if _, err := svc.Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Login(context.Background(), loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRefreshRotates(t *testing.T) {
	w := newWorld()
	first := loginOnce(t, w)
	got, err := w.service().Refresh(context.Background(), RefreshCommand{
		RefreshToken: first.RefreshToken,
		IP:           "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != first.SessionID {
		t.Fatalf("session %s %s", got.SessionID, first.SessionID)
	}
	if got.RefreshToken == "" || got.RefreshToken == first.RefreshToken {
		t.Fatalf("refresh not rotated: %q", got.RefreshToken)
	}
	if got.AccessToken == "" || got.ExpiresIn != domain.AccessExpiresIn {
		t.Fatalf("%+v", got)
	}
}

func TestRefreshReuseRevokesSession(t *testing.T) {
	w := newWorld()
	first := loginOnce(t, w)
	svc := w.service()
	second, err := svc.Refresh(context.Background(), RefreshCommand{
		RefreshToken: first.RefreshToken,
		IP:           "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Refresh(context.Background(), RefreshCommand{
		RefreshToken: first.RefreshToken,
		IP:           "203.0.113.10",
	})
	if !errors.Is(err, domain.ErrTokenReused) {
		t.Fatalf("got %v", err)
	}
	sess, _ := w.sessions.Get(context.Background(), first.SessionID)
	if sess.Live() || sess.RevokeReason != domain.RevokeTokenReuse {
		t.Fatalf("session %+v", sess)
	}
	_, err = svc.Refresh(context.Background(), RefreshCommand{
		RefreshToken: second.RefreshToken,
		IP:           "203.0.113.10",
	})
	if !errors.Is(err, domain.ErrTokenReused) && !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("new token after reuse: %v", err)
	}
}

func TestRefreshExpiredKeepsSession(t *testing.T) {
	w := newWorld()
	first := loginOnce(t, w)
	w.clock.t = w.clock.t.Add(domain.RefreshTTL).Add(time.Second)
	_, err := w.service().Refresh(context.Background(), RefreshCommand{
		RefreshToken: first.RefreshToken,
		IP:           "203.0.113.10",
	})
	if !errors.Is(err, domain.ErrTokenExpired) {
		t.Fatalf("got %v", err)
	}
	sess, _ := w.sessions.Get(context.Background(), first.SessionID)
	if !sess.Live() {
		t.Fatal("expired refresh must not revoke session")
	}
}

func TestRefreshUnknownAndEmpty(t *testing.T) {
	w := newWorld()
	svc := w.service()
	_, err := svc.Refresh(context.Background(), RefreshCommand{RefreshToken: "nope", IP: "1.1.1.1"})
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("unknown: %v", err)
	}
	_, err = svc.Refresh(context.Background(), RefreshCommand{RefreshToken: "", IP: "1.1.1.1"})
	if !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("empty: %v", err)
	}
	_, err = svc.Refresh(context.Background(), RefreshCommand{RefreshToken: "x", IP: ""})
	if !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("ip: %v", err)
	}
}

func TestRefreshRevokedSession(t *testing.T) {
	w := newWorld()
	first := loginOnce(t, w)
	if err := w.service().Logout(context.Background(), domain.Principal{
		UserID:    first.User.ID,
		SessionID: first.SessionID,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := w.service().Refresh(context.Background(), RefreshCommand{
		RefreshToken: first.RefreshToken,
		IP:           "203.0.113.10",
	})
	if !errors.Is(err, domain.ErrInvalidToken) && !errors.Is(err, domain.ErrTokenReused) {
		t.Fatalf("got %v", err)
	}
}

func TestRefreshIssuerError(t *testing.T) {
	w := newWorld()
	first := loginOnce(t, w)
	w.issuer.fail = true
	_, err := w.service().Refresh(context.Background(), RefreshCommand{
		RefreshToken: first.RefreshToken,
		IP:           "203.0.113.10",
	})
	if err == nil {
		t.Fatal("expected issue error")
	}
}

func TestRefreshCanceled(t *testing.T) {
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.service().Refresh(ctx, RefreshCommand{RefreshToken: "x", IP: "1.1.1.1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
