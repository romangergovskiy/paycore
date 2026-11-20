package accountshttp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
)

const (
	cursorVersion        byte = 1
	cursorTimeSize            = 8
	cursorUUIDSize            = 16
	cursorMACSize             = 32
	cursorPayloadSize         = 1 + cursorTimeSize + cursorUUIDSize
	cursorRawSize             = cursorPayloadSize + cursorMACSize
	cursorSecretMinBytes      = 32
	// maxStatementCursorChars bounds the HTTP query value. A v1 token is 76 chars.
	maxStatementCursorChars = 128
)

// CursorCodec HMAC-SHA256-signs statement keyset cursors for HTTP.
// It is integrity protection, not encryption: the client must not parse the token.
type CursorCodec struct {
	secret []byte
}

// NewCursorCodec copies secret. Length must be at least 32 bytes.
func NewCursorCodec(secret []byte) (*CursorCodec, error) {
	if len(secret) < cursorSecretMinBytes {
		return nil, fmt.Errorf("accounts: cursor secret must be at least %d bytes", cursorSecretMinBytes)
	}
	out := make([]byte, len(secret))
	copy(out, secret)
	return &CursorCodec{secret: out}, nil
}

// Encode returns a URL-safe Base64 HMAC token for c. Application StatementCursor is unchanged.
func (c *CursorCodec) Encode(cur application.StatementCursor) (string, error) {
	if c == nil || len(c.secret) < cursorSecretMinBytes {
		return "", domain.ErrInvalidAccount
	}
	if cur.ID == uuid.Nil || cur.CreatedAt.IsZero() {
		return "", domain.ErrInvalidAccount
	}
	payload := encodeCursorPayload(cursorVersion, cur.CreatedAt.UTC(), cur.ID)
	mac := cursorMAC(c.secret, payload)
	raw := make([]byte, 0, cursorRawSize)
	raw = append(raw, payload...)
	raw = append(raw, mac...)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Decode verifies HMAC and returns the typed application cursor.
// Empty raw is the first page (nil cursor). Tampering, unknown version,
// corrupt encoding, or invalid contents are ErrInvalidAccount (400).
func (c *CursorCodec) Decode(raw string) (*application.StatementCursor, error) {
	if raw == "" {
		return nil, nil
	}
	if c == nil || len(c.secret) < cursorSecretMinBytes {
		return nil, domain.ErrInvalidAccount
	}
	if len(raw) > maxStatementCursorChars {
		return nil, domain.ErrInvalidAccount
	}
	bin, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(bin) != cursorRawSize {
		return nil, domain.ErrInvalidAccount
	}
	payload, mac := bin[:cursorPayloadSize], bin[cursorPayloadSize:]
	if !hmac.Equal(mac, cursorMAC(c.secret, payload)) {
		return nil, domain.ErrInvalidAccount
	}
	if payload[0] != cursorVersion {
		return nil, domain.ErrInvalidAccount
	}
	nano := int64(binary.BigEndian.Uint64(payload[1 : 1+cursorTimeSize]))
	created := time.Unix(0, nano).UTC()
	var id uuid.UUID
	copy(id[:], payload[1+cursorTimeSize:])
	if id == uuid.Nil || created.IsZero() {
		return nil, domain.ErrInvalidAccount
	}
	return &application.StatementCursor{CreatedAt: created, ID: id}, nil
}

func encodeCursorPayload(version byte, created time.Time, id uuid.UUID) []byte {
	payload := make([]byte, cursorPayloadSize)
	payload[0] = version
	binary.BigEndian.PutUint64(payload[1:1+cursorTimeSize], uint64(created.UnixNano()))
	copy(payload[1+cursorTimeSize:], id[:])
	return payload
}

func cursorMAC(secret, payload []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}
