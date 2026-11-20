package accountshttp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
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

type statementCursorJSON struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
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

func encodeStatementCursor(c application.StatementCursor) (string, error) {
	payload, err := json.Marshal(statementCursorJSON{
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339Nano),
		ID:        c.ID.String(),
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

const maxStatementCursorChars = 256

func decodeStatementCursor(raw string) (*application.StatementCursor, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxStatementCursorChars {
		return nil, domain.ErrInvalidAccount
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, domain.ErrInvalidAccount
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.DisallowUnknownFields()
	var body statementCursorJSON
	if err := dec.Decode(&body); err != nil {
		return nil, domain.ErrInvalidAccount
	}
	var extra json.RawMessage
	err = dec.Decode(&extra)
	if err == nil || !errors.Is(err, io.EOF) {
		return nil, domain.ErrInvalidAccount
	}
	created, err := time.Parse(time.RFC3339Nano, body.CreatedAt)
	if err != nil {
		return nil, domain.ErrInvalidAccount
	}
	id, err := uuid.Parse(body.ID)
	if err != nil || id == uuid.Nil {
		return nil, domain.ErrInvalidAccount
	}
	return &application.StatementCursor{CreatedAt: created.UTC(), ID: id}, nil
}
