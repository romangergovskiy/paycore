package authcrypto

import (
	"testing"
	"time"
)

func TestSystemClockUTC(t *testing.T) {
	c := NewSystemClock()
	now := c.Now()
	if now.Location() != time.UTC {
		t.Fatalf("location %v", now.Location())
	}
	if now.IsZero() {
		t.Fatal("zero time")
	}
	delta := time.Since(now)
	if delta < 0 {
		delta = -delta
	}
	if delta > 5*time.Second {
		t.Fatalf("clock skew %v", delta)
	}
}
