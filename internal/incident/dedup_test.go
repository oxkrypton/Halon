package incident

import (
	"testing"
	"time"
)

func TestDeduperDetectsDuplicatesWithinTTL(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	deduper := NewDeduper(time.Minute)
	deduper.clock = func() time.Time { return now }

	if deduper.Seen("evt-1") {
		t.Fatal("first occurrence must not be a duplicate")
	}
	if !deduper.Seen("evt-1") {
		t.Fatal("second occurrence within the TTL must be a duplicate")
	}
	if deduper.Seen("evt-2") {
		t.Fatal("a different event ID must not be a duplicate")
	}

	now = now.Add(2 * time.Minute)
	if deduper.Seen("evt-1") {
		t.Fatal("event ID must be accepted again after the TTL expires")
	}
}

func TestDeduperEvictsExpiredEntries(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	deduper := NewDeduper(time.Minute)
	deduper.clock = func() time.Time { return now }

	deduper.Seen("evt-old")
	now = now.Add(2 * time.Minute)
	deduper.Seen("evt-new")

	if size := deduper.Size(); size > 2 {
		t.Fatalf("deduper size = %d, want expired entries evicted", size)
	}
}

func TestDeduperIgnoresEmptyID(t *testing.T) {
	t.Parallel()

	deduper := NewDeduper(time.Minute)
	if deduper.Seen("") {
		t.Fatal("empty event ID must never be reported as duplicate")
	}
	if deduper.Size() != 0 {
		t.Fatal("empty event ID must not be recorded")
	}
}

func TestDeduperDefaultsTTL(t *testing.T) {
	t.Parallel()

	deduper := NewDeduper(0)
	if deduper.ttl != 10*time.Minute {
		t.Fatalf("default ttl = %s, want 10m", deduper.ttl)
	}
}
