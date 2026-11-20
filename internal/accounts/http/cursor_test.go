package accountshttp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
)

func testStatementCursor() application.StatementCursor {
	return application.StatementCursor{
		CreatedAt: time.Date(2026, 11, 20, 12, 0, 0, 123, time.UTC),
		ID:        uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000001"),
	}
}

func otherCursorSecret() []byte {
	return []byte("other-accounts-cursor-secret-32b")
}

func TestCursorCodecValidRoundTrip(t *testing.T) {
	c := testCursor(t)
	cur := testStatementCursor()
	raw, err := c.Encode(cur)
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || strings.Contains(raw, cur.ID.String()) || strings.Contains(raw, "{") {
		t.Fatalf("token not opaque enough: %q", raw)
	}
	got, err := c.Decode(raw)
	if err != nil || got == nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != cur.ID || !got.CreatedAt.Equal(cur.CreatedAt) {
		t.Fatalf("got %+v want %+v", got, cur)
	}
}

func TestCursorCodecEmptyIsFirstPage(t *testing.T) {
	c := testCursor(t)
	got, err := c.Decode("")
	if err != nil || got != nil {
		t.Fatalf("empty: %+v %v", got, err)
	}
}

func TestCursorCodecTamperedTimestamp(t *testing.T) {
	assertTamper400(t, 5)
}

func TestCursorCodecTamperedUUID(t *testing.T) {
	assertTamper400(t, 12)
}

func TestCursorCodecTamperedSignature(t *testing.T) {
	assertTamper400(t, cursorRawSize-1)
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
	cur := testStatementCursor()
	payload := encodeCursorPayload(2, cur.CreatedAt, cur.ID)
	mac := hmac.New(sha256.New, []byte(testCursorSecret))
	_, _ = mac.Write(payload)
	raw := append(payload, mac.Sum(nil)...)
	tok := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := c.Decode(tok); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("v2: %v", err)
	}
}

func TestCursorCodecWrongSecret(t *testing.T) {
	a := testCursor(t)
	b, err := NewCursorCodec(otherCursorSecret())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.Encode(testStatementCursor())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Decode(raw); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("other secret: %v", err)
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

func TestCursorCodecBoundInputSize(t *testing.T) {
	c := testCursor(t)
	huge := strings.Repeat("A", maxStatementCursorChars+1)
	if _, err := c.Decode(huge); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("oversized: %v", err)
	}
}

func TestNewCursorCodecRequiresSecret(t *testing.T) {
	_, err := NewCursorCodec([]byte(testCursorSecret[:31]))
	if err == nil {
		t.Fatal("short secret accepted")
	}
	if strings.Contains(err.Error(), testCursorSecret) {
		t.Fatalf("secret leaked: %v", err)
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
