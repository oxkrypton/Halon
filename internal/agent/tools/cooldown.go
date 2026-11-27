package tools

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CooldownStore arbitrates whether a mutating action may proceed on a
// resource key. Claim reserves key for ttl; when the key is already held it
// reports the remaining wait instead.
type CooldownStore interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (remaining time.Duration, claimed bool, err error)
}

// ErrCooldownUnavailable marks claim attempts that failed because the backing
// store is unreachable. Mutation tools surface it as an error so safety
// cooldowns never fail open.
var ErrCooldownUnavailable = fmt.Errorf("cooldown store unavailable")

const maxDeadlinesBeforeSweep = 64

// MemoryCooldownStore backs CooldownStore with an in-process concurrency-safe
// expiration table suitable for single-instance agent deployments.
type MemoryCooldownStore struct {
	mu        sync.Mutex
	deadlines map[string]time.Time
	now       func() time.Time
}

var _ CooldownStore = (*MemoryCooldownStore)(nil)

func NewMemoryCooldownStore() *MemoryCooldownStore {
	return NewMemoryCooldownStoreWithClock(time.Now)
}

func NewMemoryCooldownStoreWithClock(now func() time.Time) *MemoryCooldownStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryCooldownStore{
		deadlines: make(map[string]time.Time),
		now:       now,
	}
}

func (s *MemoryCooldownStore) Claim(_ context.Context, key string, ttl time.Duration) (time.Duration, bool, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	if expiry, ok := s.deadlines[key]; ok && expiry.After(now) {
		return expiry.Sub(now), false, nil
	}
	if len(s.deadlines) >= maxDeadlinesBeforeSweep {
		for k, expiry := range s.deadlines {
			if !expiry.After(now) {
				delete(s.deadlines, k)
			}
		}
	}
	s.deadlines[key] = now.Add(ttl)
	return 0, true, nil
}

type resourceLockEntry struct {
	mu   sync.Mutex
	refs int
}

// ResourceLocker serializes concurrent mutating operations that target the
// same canonical resource key across sessions while allowing operations on
// distinct resources to run concurrently.
type ResourceLocker struct {
	mu      sync.Mutex
	entries map[string]*resourceLockEntry
}

func NewResourceLocker() *ResourceLocker {
	return &ResourceLocker{
		entries: make(map[string]*resourceLockEntry),
	}
}

func (l *ResourceLocker) Lock(key string) func() {
	l.mu.Lock()
	entry, ok := l.entries[key]
	if !ok {
		entry = &resourceLockEntry{}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()
			l.mu.Lock()
			entry.refs--
			if entry.refs == 0 && l.entries[key] == entry {
				delete(l.entries, key)
			}
			l.mu.Unlock()
		})
	}
}
