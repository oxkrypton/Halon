package incident

import (
	"sync"
	"time"
)

// Deduper tracks recently seen event IDs so duplicate timeout
// notifications are only investigated once within the TTL window.
// It is safe for concurrent use.
type Deduper struct {
	mu    sync.Mutex
	ttl   time.Duration
	clock func() time.Time
	seen  map[string]time.Time
}

func NewDeduper(ttl time.Duration) *Deduper {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &Deduper{ttl: ttl, clock: time.Now, seen: make(map[string]time.Time)}
}

// Seen reports whether eventID was already recorded within the TTL.
// New IDs are recorded; expired entries are evicted lazily.
func (d *Deduper) Seen(eventID string) bool {
	if eventID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	if expiresAt, ok := d.seen[eventID]; ok && now.Before(expiresAt) {
		return true
	}
	d.seen[eventID] = now.Add(d.ttl)
	d.evictLocked(now)
	return false
}

// Size reports how many event IDs are currently tracked, including
// entries that have not been lazily evicted yet.
func (d *Deduper) Size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

func (d *Deduper) evictLocked(now time.Time) {
	// Evict on a power-of-two boundary to keep inserts amortized O(1).
	if len(d.seen) < 1024 && len(d.seen)&(len(d.seen)-1) != 0 {
		return
	}
	for id, expiresAt := range d.seen {
		if !now.Before(expiresAt) {
			delete(d.seen, id)
		}
	}
}
