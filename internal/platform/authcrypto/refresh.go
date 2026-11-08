package authcrypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/romangergovskiy/paycore/internal/auth/application"
)

var _ application.RefreshIssuer = OpaqueRefresh{}

const refreshRawLen = 32

var errInvalidRefresh = errors.New("auth: refresh token is invalid")

// OpaqueRefresh implements application.RefreshIssuer. Plaintext is
// unpadded base64url of 32 random bytes; the stored hash is SHA-256 of
// those raw bytes, not of the encoded string.
type OpaqueRefresh struct{}

func NewOpaqueRefresh() OpaqueRefresh { return OpaqueRefresh{} }

func (OpaqueRefresh) Issue() (string, []byte, error) {
	raw := make([]byte, refreshRawLen)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: generate refresh token: %w", err)
	}
	sum := sha256.Sum256(raw)
	hash := make([]byte, sha256.Size)
	copy(hash, sum[:])
	return base64.RawURLEncoding.EncodeToString(raw), hash, nil
}

func (OpaqueRefresh) Hash(plaintext string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(plaintext)
	if err != nil || len(raw) != refreshRawLen {
		return nil, errInvalidRefresh
	}
	sum := sha256.Sum256(raw)
	hash := make([]byte, sha256.Size)
	copy(hash, sum[:])
	return hash, nil
}
