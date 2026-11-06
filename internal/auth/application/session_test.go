package application

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func TestLogoutAndRepeat(t *testing.T) {
	w := newWorld()
	first := loginOnce(t, w)
	p := domain.Principal{UserID: first.User.ID, SessionID: first.SessionID}
	svc := w.service()
	if err := svc.Logout(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(context.Background(), p); err != nil {
		t.Fatalf("repeat logout: %v", err)
	}
	_, err := svc.ValidateSession(context.Background(), first.SessionID)
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("after logout: %v", err)
	}
}

func TestLogoutUnknownSession(t *testing.T) {
	w := newWorld()
	err := w.service().Logout(context.Background(), domain.Principal{
		UserID:    uuid.Must(uuid.NewV7()),
		SessionID: uuid.Must(uuid.NewV7()),
	})
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestLogoutOtherUsersSession(t *testing.T) {
	w := newWorld()
	first := loginOnce(t, w)
	err := w.service().Logout(context.Background(), domain.Principal{
		UserID:    uuid.Must(uuid.NewV7()),
		SessionID: first.SessionID,
	})
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestValidateSessionLive(t *testing.T) {
	w := newWorld()
	first := loginOnce(t, w)
	p, err := w.service().ValidateSession(context.Background(), first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != first.User.ID || p.SessionID != first.SessionID || !p.Has(domain.RoleCustomer) {
		t.Fatalf("%+v", p)
	}
}

func TestValidateSessionRevoked(t *testing.T) {
	w := newWorld()
	first := loginOnce(t, w)
	_ = w.service().Logout(context.Background(), domain.Principal{
		UserID: first.User.ID, SessionID: first.SessionID,
	})
	_, err := w.service().ValidateSession(context.Background(), first.SessionID)
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestValidateSessionUnknownAndNil(t *testing.T) {
	w := newWorld()
	svc := w.service()
	_, err := svc.ValidateSession(context.Background(), uuid.Must(uuid.NewV7()))
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("unknown: %v", err)
	}
	_, err = svc.ValidateSession(context.Background(), uuid.Nil)
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("nil: %v", err)
	}
}

func TestValidateSessionInactiveUser(t *testing.T) {
	w := newWorld()
	first := loginOnce(t, w)
	u, _ := w.users.Get(context.Background(), first.User.ID)
	u.Status = domain.StatusLocked
	now := w.clock.t
	u.LockedAt = &now
	_ = w.users.Update(context.Background(), u)
	_, err := w.service().ValidateSession(context.Background(), first.SessionID)
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestLogoutCanceled(t *testing.T) {
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := w.service().Logout(ctx, domain.Principal{SessionID: uuid.Must(uuid.NewV7())})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestValidateSessionCanceled(t *testing.T) {
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.service().ValidateSession(ctx, uuid.Must(uuid.NewV7()))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
