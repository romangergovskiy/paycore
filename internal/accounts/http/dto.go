package accountshttp

import (
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
)

type accountDTO struct {
	ID            uuid.UUID `json:"id"`
	AccountNumber string    `json:"account_number"`
	Purpose       string    `json:"purpose"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	Name          string    `json:"name"`
	CreatedAt     time.Time `json:"created_at"`
}

type accountsResponse struct {
	Accounts []accountDTO `json:"accounts"`
}

type balanceResponse struct {
	AccountID uuid.UUID `json:"account_id"`
	Currency  string    `json:"currency"`
	Available int64     `json:"available"`
	Reserved  int64     `json:"reserved"`
	Version   int64     `json:"version"`
}

type statementLineDTO struct {
	Kind         string    `json:"kind"`
	Side         string    `json:"side"`
	Amount       int64     `json:"amount"`
	Currency     string    `json:"currency"`
	BalanceAfter *int64    `json:"balance_after"`
	CreatedAt    time.Time `json:"created_at"`
	PostedAt     time.Time `json:"posted_at"`
}

type statementResponse struct {
	Lines      []statementLineDTO `json:"lines"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

func accountDTOFrom(acc domain.Account) accountDTO {
	return accountDTO{
		ID:            acc.ID,
		AccountNumber: acc.AccountNumber,
		Purpose:       string(acc.Purpose),
		Currency:      string(acc.Currency),
		Status:        string(acc.Status),
		Name:          acc.Name,
		CreatedAt:     acc.CreatedAt,
	}
}

func accountDTOs(in []domain.Account) []accountDTO {
	out := make([]accountDTO, 0, len(in))
	for _, acc := range in {
		out = append(out, accountDTOFrom(acc))
	}
	return out
}

func statementLineDTOs(in []application.StatementLine) []statementLineDTO {
	out := make([]statementLineDTO, 0, len(in))
	for _, line := range in {
		out = append(out, statementLineDTO{
			Kind:         string(line.Kind),
			Side:         string(line.Side),
			Amount:       line.Amount,
			Currency:     line.Currency,
			BalanceAfter: line.BalanceAfter,
			CreatedAt:    line.CreatedAt,
			PostedAt:     line.PostedAt,
		})
	}
	return out
}
