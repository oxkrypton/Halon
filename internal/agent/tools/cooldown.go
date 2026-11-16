package tools

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// CooldownStore arbitrates which replica may execute a mutating action.
// Claim reserves key for ttl; when the key is already held it reports the
// remaining wait instead. The single-replica memory mode uses
// memoryCooldownStore; the shared multi-replica mode uses
// RedisCooldownStore, where SET NX PX plays the same arbitration role as
// incident.Store.Claim's ON CONFLICT DO NOTHING.
type CooldownStore interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (remaining time.Duration, claimed bool, err error)
}

// ErrCooldownUnavailable marks claim attempts that failed because the
// backing store is unreachable. Mutation tools surface it as an error:
// a safety feature must never fail open.
var ErrCooldownUnavailable = fmt.Errorf("cooldown store unavailable")

// memoryCooldownStore is the single-replica CooldownStore (the pre-shared
// mode behavior): a map guarded by a mutex.
type memoryCooldownStore struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newMemoryCooldownStore() *memoryCooldownStore {
	return &memoryCooldownStore{last: make(map[string]time.Time)}
}

func (m *memoryCooldownStore) Claim(_ context.Context, key string, ttl time.Duration) (time.Duration, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if last, ok := m.last[key]; ok && now.Sub(last) < ttl {
		return ttl - now.Sub(last), false, nil
	}
	m.last[key] = now
	return 0, true, nil
}

// RedisCooldownStore is the shared-mode CooldownStore. Claims are atomic
// SET NX PX commands, so N replicas together execute a mutating action at
// most once per cooldown window. A background pinger keeps the health hook
// current because mutations can be hours apart.
type RedisCooldownStore struct {
	client    *redis.Client
	logger    *slog.Logger
	onHealth  func(bool)
	opTimeout time.Duration

	done chan struct{}
	wg   sync.WaitGroup
}

var _ CooldownStore = (*RedisCooldownStore)(nil)

func NewRedisCooldownStore(client *redis.Client, opTimeout time.Duration, logger *slog.Logger, onHealth func(bool)) *RedisCooldownStore {
	if opTimeout <= 0 {
		opTimeout = 500 * time.Millisecond
	}
	if logger == nil {
		logger = slog.Default()
	}
	store := &RedisCooldownStore{
		client: client, logger: logger, onHealth: onHealth, opTimeout: opTimeout,
		done: make(chan struct{}),
	}
	store.wg.Add(1)
	go store.pingLoop()
	return store
}

func (s *RedisCooldownStore) Claim(ctx context.Context, key string, ttl time.Duration) (time.Duration, bool, error) {
	opContext, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
	claimed, err := s.client.SetNX(opContext, cooldownKeyPrefix+key, 1, ttl).Result()
	if err != nil {
		s.reportHealth(false)
		return 0, false, fmt.Errorf("%w: %v", ErrCooldownUnavailable, err)
	}
	s.reportHealth(true)
	if claimed {
		return 0, true, nil
	}
	ttlContext, cancelTTL := context.WithTimeout(ctx, s.opTimeout)
	defer cancelTTL()
	remaining, err := s.client.PTTL(ttlContext, cooldownKeyPrefix+key).Result()
	if err != nil {
		s.reportHealth(false)
		return 0, false, fmt.Errorf("%w: %v", ErrCooldownUnavailable, err)
	}
	s.reportHealth(true)
	return remaining, false, nil
}

// Close stops the background pinger.
func (s *RedisCooldownStore) Close() {
	close(s.done)
	s.wg.Wait()
}

func (s *RedisCooldownStore) pingLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), s.opTimeout)
			s.reportHealth(s.client.Ping(ctx).Err() == nil)
			cancel()
		}
	}
}

func (s *RedisCooldownStore) reportHealth(up bool) {
	if s.onHealth != nil {
		s.onHealth(up)
	}
}

const cooldownKeyPrefix = "halon:cooldown:"
