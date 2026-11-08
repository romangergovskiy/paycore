// Package authcrypto implements Identity/Auth infrastructure: Argon2id
// hashing, JWT HS256 access tokens, opaque refresh tokens, and a UTC clock.
// Domain and application must not import this package.
package authcrypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/romangergovskiy/paycore/internal/auth/application"
)

var _ application.Hasher = Argon2idHasher{}

const (
	argonMemoryKiB  = 19456
	argonIterations = 2
	argonParallel   = 1
	argonSaltLen    = 16
	argonKeyLen     = 32
	argonVersion    = argon2.Version

	// Caps on parameters parsed from a stored PHC string. Larger values
	// are rejected so a crafted row cannot DoS Verify even before the
	// D-41 exact-match check.
	maxArgonMemoryKiB  = 64 * 1024
	maxArgonIterations = 4
	maxArgonParallel   = 4
)

// dummyPHC is a precomputed Argon2id hash of an unused password. Login of an
// unknown email Verify()s against it so the CPU cost matches a real user.
const dummyPHC = "$argon2id$v=19$m=19456,t=2,p=1$cGF5Y29yZS1kdW1teS0xNg$jqoWTfRQ64xQMhdCUzMQAlFgF9MuueyMrkUvZ9p21gw"

var (
	errInvalidPHC     = errors.New("auth: password hash is not a valid argon2id PHC string")
	errUnsupportedPHC = errors.New("auth: password hash parameters are not supported")
)

// Argon2idHasher implements application.Hasher.
type Argon2idHasher struct{}

func NewArgon2idHasher() Argon2idHasher { return Argon2idHasher{} }

func (Argon2idHasher) Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonIterations, argonMemoryKiB, argonParallel, argonKeyLen)
	return encodePHC(salt, key, argonMemoryKiB, argonIterations, argonParallel), nil
}

func (Argon2idHasher) Verify(password, encoded string) (bool, error) {
	p, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), p.salt, p.time, p.memory, p.threads, uint32(len(p.key)))
	if subtle.ConstantTimeCompare(got, p.key) == 1 {
		return true, nil
	}
	return false, nil
}

func (Argon2idHasher) DummyHash() string { return dummyPHC }

func encodePHC(salt, key []byte, memory, time uint32, threads uint8) string {
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, memory, time, threads, enc.EncodeToString(salt), enc.EncodeToString(key))
}

type parsedPHC struct {
	memory, time uint32
	threads      uint8
	salt, key    []byte
}

func parsePHC(encoded string) (parsedPHC, error) {
	var zero parsedPHC
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return zero, errInvalidPHC
	}
	if parts[2] != "v=19" {
		return zero, errUnsupportedPHC
	}

	var memory, time uint32
	var threads uint8
	seen := map[string]bool{}
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || v == "" {
			return zero, errInvalidPHC
		}
		if seen[k] {
			return zero, errInvalidPHC
		}
		seen[k] = true
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return zero, errInvalidPHC
		}
		switch k {
		case "m":
			memory = uint32(n)
		case "t":
			time = uint32(n)
		case "p":
			if n > 255 {
				return zero, errUnsupportedPHC
			}
			threads = uint8(n)
		default:
			return zero, errInvalidPHC
		}
	}
	if !seen["m"] || !seen["t"] || !seen["p"] {
		return zero, errInvalidPHC
	}
	if memory > maxArgonMemoryKiB || time > maxArgonIterations || uint32(threads) > maxArgonParallel {
		return zero, errUnsupportedPHC
	}
	if memory != argonMemoryKiB || time != argonIterations || threads != argonParallel {
		return zero, errUnsupportedPHC
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != argonSaltLen {
		return zero, errInvalidPHC
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) != argonKeyLen {
		return zero, errInvalidPHC
	}
	return parsedPHC{memory: memory, time: time, threads: threads, salt: salt, key: key}, nil
}
