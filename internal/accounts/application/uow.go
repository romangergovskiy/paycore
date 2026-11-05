package application

import (
	"context"

	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
)

// Deps are the ports of one Accounts write. Store and Opener must share
// the same database transaction; the UnitOfWork implementation wires them.
type Deps struct {
	Store  Store
	Opener ledger.AccountOpener
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
