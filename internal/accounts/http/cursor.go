package accountshttp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
)

const (
	cursorVersion   byte = 1
	cursorTimeSize       = 8
	cursorUUIDSize       = 16
	cursorNonceSize      = 12
	cursorTagSize        = 16
	cursorPlainSize      = cursorTimeSize + cursorUUIDSize
	cursorRawSize        = 1 + cursorNonceSize + cursorPlainSize + cursorTagSize
	cursorKeyBytes       = 32
	// maxStatementCursorChars bounds the HTTP query value. A v1 token is 71 chars.
	maxStatementCursorChars = 128
)

// CursorCodec encrypts statement keyset cursors with AES-256-GCM for HTTP.
// Application StatementCursor is unchanged.
type CursorCodec struct {
	gcm cipher.AEAD
}

// NewCursorCodec builds an AES-256-GCM codec. key must be exactly 32 bytes.
func NewCursorCodec(key []byte) (*CursorCodec, error) {
	if len(key) != cursorKeyBytes {
		return nil, fmt.Errorf("accounts: cursor secret must be exactly %d bytes", cursorKeyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("accounts: cursor cipher")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("accounts: cursor gcm")
	}
	if gcm.NonceSize() != cursorNonceSize || gcm.Overhead() != cursorTagSize {
		return nil, fmt.Errorf("accounts: cursor gcm")
	}
	return &CursorCodec{gcm: gcm}, nil
}

// Encode returns a URL-safe Base64 AES-256-GCM token. A fresh nonce is used
// every time. Application StatementCursor is unchanged.
func (c *CursorCodec) Encode(cur application.StatementCursor) (string, error) {
	if c == nil || c.gcm == nil {
		return "", domain.ErrInvalidAccount
	}
	if cur.ID == uuid.Nil || cur.CreatedAt.IsZero() {
		return "", domain.ErrInvalidAccount
	}
	plain := encodeCursorPlaintext(cur.CreatedAt.UTC(), cur.ID)
	nonce := make([]byte, cursorNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", domain.ErrInvalidAccount
	}
	aad := []byte{cursorVersion}
	sealed := c.gcm.Seal(nil, nonce, plain, aad)
	raw := make([]byte, 0, cursorRawSize)
	raw = append(raw, cursorVersion)
	raw = append(raw, nonce...)
	raw = append(raw, sealed...)
	if len(raw) != cursorRawSize {
		return "", domain.ErrInvalidAccount
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Decode decrypts and authenticates an HTTP cursor.
// Empty raw is the first page (nil cursor). Tampering, unknown version,
// corrupt encoding, or invalid contents are ErrInvalidAccount (400).
func (c *CursorCodec) Decode(raw string) (*application.StatementCursor, error) {
	if raw == "" {
		return nil, nil
	}
	if c == nil || c.gcm == nil {
		return nil, domain.ErrInvalidAccount
	}
	if len(raw) > maxStatementCursorChars {
		return nil, domain.ErrInvalidAccount
	}
	bin, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(bin) != cursorRawSize {
		return nil, domain.ErrInvalidAccount
	}
	if bin[0] != cursorVersion {
		return nil, domain.ErrInvalidAccount
	}
	nonce := bin[1 : 1+cursorNonceSize]
	sealed := bin[1+cursorNonceSize:]
	plain, err := c.gcm.Open(nil, nonce, sealed, []byte{cursorVersion})
	if err != nil || len(plain) != cursorPlainSize {
		return nil, domain.ErrInvalidAccount
	}
	nano := int64(binary.BigEndian.Uint64(plain[:cursorTimeSize]))
	created := time.Unix(0, nano).UTC()
	var id uuid.UUID
	copy(id[:], plain[cursorTimeSize:])
	if id == uuid.Nil || created.IsZero() {
		return nil, domain.ErrInvalidAccount
	}
	return &application.StatementCursor{CreatedAt: created, ID: id}, nil
}

func encodeCursorPlaintext(created time.Time, id uuid.UUID) []byte {
	plain := make([]byte, cursorPlainSize)
	binary.BigEndian.PutUint64(plain[:cursorTimeSize], uint64(created.UnixNano()))
	copy(plain[cursorTimeSize:], id[:])
	return plain
}
