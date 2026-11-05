package application

import (
	"context"

	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
)

// Deps are the ports of one Accounts write. Store, Opener, and Locker must
// share the same database transaction; the UnitOfWork implementation wires them.
type Deps struct {
	Store  Store
	Opener ledger.AccountOpener
	Locker ledger.BalanceLocker
}

// UnitOfWork runs fn in one transaction. The implementation Begins, builds
// Deps on that transaction, then Commits only if fn returns nil and ctx is
// still valid. Any error, including ctx cancellation after fn, rolls back.
// Application and domain do not Begin, Commit, or Rollback.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(ctx context.Context, d Deps) error) error
}

// OpenIn opens a business account and its ledger pair inside uow.
func OpenIn(ctx context.Context, uow UnitOfWork, cmd domain.OpenCommand) (Opened, error) {
	var out Opened
	if uow == nil {
		return out, errNotWired
	}
	err := uow.Do(ctx, func(ctx context.Context, d Deps) error {
		var err error
		out, err = Service{Store: d.Store, Opener: d.Opener}.Open(ctx, cmd)
		return err
	})
	return out, err
}

// BlockIn sets ACTIVE → BLOCKED inside uow. It does not lock balances.
func BlockIn(ctx context.Context, uow UnitOfWork, cmd domain.StatusCommand) (domain.Account, error) {
	var out domain.Account
	if uow == nil {
		return out, errNotWired
	}
	err := uow.Do(ctx, func(ctx context.Context, d Deps) error {
		var err error
		out, err = Service{Store: d.Store}.Block(ctx, cmd)
		return err
	})
	return out, err
}

// UnblockIn sets BLOCKED → ACTIVE inside uow. It does not lock balances.
func UnblockIn(ctx context.Context, uow UnitOfWork, cmd domain.StatusCommand) (domain.Account, error) {
	var out domain.Account
	if uow == nil {
		return out, errNotWired
	}
	err := uow.Do(ctx, func(ctx context.Context, d Deps) error {
		var err error
		out, err = Service{Store: d.Store}.Unblock(ctx, cmd)
		return err
	})
	return out, err
}

// CloseIn sets ACTIVE|BLOCKED → CLOSED inside uow after L5 shows empty balances.
func CloseIn(ctx context.Context, uow UnitOfWork, cmd domain.StatusCommand) (domain.Account, error) {
	var out domain.Account
	if uow == nil {
		return out, errNotWired
	}
	err := uow.Do(ctx, func(ctx context.Context, d Deps) error {
		var err error
		out, err = Service{Store: d.Store, Locker: d.Locker}.Close(ctx, cmd)
		return err
	})
	return out, err
}
