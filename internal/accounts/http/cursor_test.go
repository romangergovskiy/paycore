package accountshttp

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/platform/apperror"
)

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) {
	return 0, r.err
}

func testStatementCursor() application.StatementCursor {
	return application.StatementCursor{
		CreatedAt: time.Date(2026, 11, 20, 12, 0, 0, 123, time.UTC),
		ID:        uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000001"),
	}
}

func otherCursorKey() []byte {
	return []byte("other-accounts-cursor-secret-32b")
}

func TestCursorCodecValidRoundTrip(t *testing.T) {
	c := testCursor(t)
	cur := testStatementCursor()
	raw, err := c.Encode(cur)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Decode(raw)
	if err != nil || got == nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.CreatedAt.Equal(cur.CreatedAt) {
		t.Fatalf("timestamp got %s want %s", got.CreatedAt, cur.CreatedAt)
	}
	if got.ID != cur.ID {
		t.Fatalf("uuid got %s want %s", got.ID, cur.ID)
	}
}

func TestCursorCodecConfidentiality(t *testing.T) {
	c := testCursor(t)
	cur := testStatementCursor()
	raw, err := c.Encode(cur)
	if err != nil {
		t.Fatal(err)
	}
	bin, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bin, cur.ID[:]) {
		t.Fatal("uuid plaintext after Base64 decode")
	}
	ts := make([]byte, cursorTimeSize)
	binary.BigEndian.PutUint64(ts, uint64(cur.CreatedAt.UnixNano()))
	if bytes.Contains(bin, ts) {
		t.Fatal("timestamp plaintext after Base64 decode")
	}
}

func TestCursorCodecFreshNonce(t *testing.T) {
	c := testCursor(t)
	cur := testStatementCursor()
	a, err := c.Encode(cur)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encode(cur)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("same payload produced identical tokens")
	}
	ab, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ab[1:1+cursorNonceSize], bb[1:1+cursorNonceSize]) {
		t.Fatal("nonce reused")
	}
	if bytes.Equal(ab[1+cursorNonceSize:], bb[1+cursorNonceSize:]) {
		t.Fatal("ciphertext reused")
	}
}

func TestCursorCodecTamperedCiphertext(t *testing.T) {
	assertTamper400(t, 1+cursorNonceSize)
}

func TestCursorCodecTamperedNonce(t *testing.T) {
	assertTamper400(t, 1)
}

func TestCursorCodecEmptyIsFirstPage(t *testing.T) {
	c := testCursor(t)
	got, err := c.Decode("")
	if err != nil || got != nil {
		t.Fatalf("empty: %+v %v", got, err)
	}
}

func TestCursorCodecCorruptBase64(t *testing.T) {
	c := testCursor(t)
	if _, err := c.Decode("%%%not-base64%%%"); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("corrupt: %v", err)
	}
	if _, err := c.Decode("not-a-cursor"); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("junk: %v", err)
	}
}

func TestCursorCodecUnknownVersion(t *testing.T) {
	c := testCursor(t)
	raw, err := c.Encode(testStatementCursor())
	if err != nil {
		t.Fatal(err)
	}
	bin, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	bin[0] = 2
	tok := base64.RawURLEncoding.EncodeToString(bin)
	if _, err := c.Decode(tok); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("v2: %v", err)
	}
}

func TestCursorCodecWrongKey(t *testing.T) {
	a := testCursor(t)
	b, err := NewCursorCodec(otherCursorKey())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.Encode(testStatementCursor())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Decode(raw); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("other key: %v", err)
	}
}

func TestCursorCodecRejectsUnsignedJSON(t *testing.T) {
	c := testCursor(t)
	cur := testStatementCursor()
	payload, err := json.Marshal(map[string]string{
		"created_at": cur.CreatedAt.UTC().Format(time.RFC3339Nano),
		"id":         cur.ID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy := base64.RawURLEncoding.EncodeToString(payload)
	if _, err := c.Decode(legacy); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("legacy unsigned: %v", err)
	}
}

func TestCursorCodecRejectsHMAC(t *testing.T) {
	c := testCursor(t)
	cur := testStatementCursor()
	payload := make([]byte, 1+cursorTimeSize+cursorUUIDSize)
	payload[0] = 1
	binary.BigEndian.PutUint64(payload[1:1+cursorTimeSize], uint64(cur.CreatedAt.UnixNano()))
	copy(payload[1+cursorTimeSize:], cur.ID[:])
	mac := hmac.New(sha256.New, []byte(testCursorSecret))
	_, _ = mac.Write(payload)
	legacy := base64.RawURLEncoding.EncodeToString(append(payload, mac.Sum(nil)...))
	if _, err := c.Decode(legacy); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("legacy HMAC: %v", err)
	}
}

func TestCursorCodecBoundInputSize(t *testing.T) {
	c := testCursor(t)
	huge := strings.Repeat("A", maxStatementCursorChars+1)
	if _, err := c.Decode(huge); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("oversized: %v", err)
	}
	short := base64.RawURLEncoding.EncodeToString([]byte{cursorVersion, 1, 2, 3})
	if _, err := c.Decode(short); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("too short: %v", err)
	}
	long := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, cursorRawSize+1))
	if _, err := c.Decode(long); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("too long: %v", err)
	}
}

func TestCursorCodecNonceFailureIsInternal(t *testing.T) {
	c := testCursor(t)
	c.nonceRand = errReader{err: io.ErrUnexpectedEOF}
	_, err := c.Encode(testStatementCursor())
	if err == nil {
		t.Fatal("expected nonce error")
	}
	if errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatal("nonce failure mapped to ErrInvalidAccount")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unwrap: %v", err)
	}
	mapped := mapError(err)
	var e *apperror.Error
	if !errors.As(mapped, &e) {
		t.Fatalf("mapError type %T", mapped)
	}
	if e.Kind != apperror.KindInternal || e.Code != apperror.CodeInternal {
		t.Fatalf("kind=%v code=%s", e.Kind, e.Code)
	}
	if e.PublicMessage() != "Internal server error." {
		t.Fatalf("public = %q", e.PublicMessage())
	}
	if strings.Contains(e.PublicMessage(), "nonce") || strings.Contains(e.PublicMessage(), "EOF") {
		t.Fatalf("leaked details: %q", e.PublicMessage())
	}
}

func TestNewCursorCodecRequiresKey(t *testing.T) {
	_, err := NewCursorCodec([]byte(testCursorSecret[:31]))
	if err == nil {
		t.Fatal("short key accepted")
	}
	if strings.Contains(err.Error(), testCursorSecret) {
		t.Fatalf("secret leaked: %v", err)
	}
	_, err = NewCursorCodec(append([]byte(testCursorSecret), 'x'))
	if err == nil {
		t.Fatal("long key accepted")
	}
}

func TestCursorCodecPaginationNoGaps(t *testing.T) {
	c := testCursor(t)
	id1 := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000001")
	id2 := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000002")
	id3 := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000003")
	t1 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	pages := []application.StatementCursor{
		{CreatedAt: t1, ID: id1},
		{CreatedAt: t1, ID: id2},
		{CreatedAt: t1.Add(time.Second), ID: id3},
	}
	seen := map[uuid.UUID]bool{}
	var prev *application.StatementCursor
	for i, want := range pages {
		raw, err := c.Encode(want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Decode(raw)
		if err != nil || got == nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if seen[got.ID] {
			t.Fatalf("duplicate %s", got.ID)
		}
		seen[got.ID] = true
		if got.ID != want.ID || !got.CreatedAt.Equal(want.CreatedAt) {
			t.Fatalf("page %d: %+v", i, got)
		}
		if prev != nil {
			if got.CreatedAt.Before(prev.CreatedAt) || (got.CreatedAt.Equal(prev.CreatedAt) && got.ID.String() <= prev.ID.String()) {
				t.Fatalf("order break prev=%+v got=%+v", prev, got)
			}
		}
		prev = got
	}
	if len(seen) != 3 {
		t.Fatalf("seen %d", len(seen))
	}
}

func assertTamper400(t *testing.T, offset int) {
	t.Helper()
	c := testCursor(t)
	raw, err := c.Encode(testStatementCursor())
	if err != nil {
		t.Fatal(err)
	}
	bin, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	bin[offset] ^= 0x01
	tampered := base64.RawURLEncoding.EncodeToString(bin)
	if _, err := c.Decode(tampered); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("offset %d: %v", offset, err)
	}
}
