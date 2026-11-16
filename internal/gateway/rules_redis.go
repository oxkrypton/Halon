package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	blacklistSetKey         = "halon:blacklist"
	blacklistInvalidateChan = "halon:blacklist:invalidate"
)

// RedisBlacklistStore is the shared-mode BlacklistStore. Redis is the fact
// source; the in-process map is only a read cache so the per-request hot
// path never pays a Redis round trip. Writes go straight to Redis and then
// fan out a pub/sub invalidation; a TTL refresh covers lost messages.
//
// Degradation: when Redis is unreachable, reads fall back to the last known
// cache and writes fail with ErrBlacklistUnavailable (mapped to 503) — a
// security feature must never appear to have succeeded when it has not.
type RedisBlacklistStore struct {
	client    *redis.Client
	logger    *slog.Logger
	onHealth  func(bool)
	ttl       time.Duration
	opTimeout time.Duration

	mu      sync.RWMutex
	blocked map[string]struct{}

	done chan struct{}
	wg   sync.WaitGroup
}

var _ BlacklistStore = (*RedisBlacklistStore)(nil)

func NewRedisBlacklistStore(client *redis.Client, ttl, opTimeout time.Duration, logger *slog.Logger, onHealth func(bool)) *RedisBlacklistStore {
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	if opTimeout <= 0 {
		opTimeout = 500 * time.Millisecond
	}
	if logger == nil {
		logger = slog.Default()
	}
	store := &RedisBlacklistStore{
		client: client, logger: logger, onHealth: onHealth,
		ttl: ttl, opTimeout: opTimeout,
		blocked: make(map[string]struct{}),
		done:    make(chan struct{}),
	}
	store.refresh()
	store.wg.Add(2)
	go store.refreshLoop()
	go store.subscribeLoop()
	return store
}

// Propagation documents the worst-case delay before a write is visible on
// every replica; the admin API includes it in write responses.
func (s *RedisBlacklistStore) Propagation() string {
	return fmt.Sprintf("eventually, <=%s", s.ttl)
}

func (s *RedisBlacklistStore) Add(ip string) (bool, error) {
	canonical, err := canonicalIP(ip)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.opTimeout)
	defer cancel()
	added, err := s.client.SAdd(ctx, blacklistSetKey, canonical).Result()
	if err != nil {
		s.reportHealth(false)
		return false, fmt.Errorf("%w: %v", ErrBlacklistUnavailable, err)
	}
	s.reportHealth(true)
	s.mu.Lock()
	s.blocked[canonical] = struct{}{}
	s.mu.Unlock()
	s.publishInvalidate()
	return added > 0, nil
}

func (s *RedisBlacklistStore) Remove(ip string) (bool, error) {
	canonical, err := canonicalIP(ip)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.opTimeout)
	defer cancel()
	removed, err := s.client.SRem(ctx, blacklistSetKey, canonical).Result()
	if err != nil {
		s.reportHealth(false)
		return false, fmt.Errorf("%w: %v", ErrBlacklistUnavailable, err)
	}
	s.reportHealth(true)
	s.mu.Lock()
	delete(s.blocked, canonical)
	s.mu.Unlock()
	s.publishInvalidate()
	return removed > 0, nil
}

func (s *RedisBlacklistStore) Contains(ip string) bool {
	canonical, err := canonicalIP(ip)
	if err != nil {
		return false
	}
	s.mu.RLock()
	_, blocked := s.blocked[canonical]
	s.mu.RUnlock()
	return blocked
}

func (s *RedisBlacklistStore) List() []string {
	s.mu.RLock()
	ips := make([]string, 0, len(s.blocked))
	for ip := range s.blocked {
		ips = append(ips, ip)
	}
	s.mu.RUnlock()
	sort.Strings(ips)
	return ips
}

// Close stops the background refresher and subscriber.
func (s *RedisBlacklistStore) Close() {
	close(s.done)
	s.wg.Wait()
}

// refresh replaces the cache with the fact source. On failure the last
// known cache stays in effect (degraded mode).
func (s *RedisBlacklistStore) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), s.opTimeout)
	defer cancel()
	members, err := s.client.SMembers(ctx, blacklistSetKey).Result()
	if err != nil {
		s.logger.Warn("blacklist refresh failed; keeping last known cache", "error", err)
		s.reportHealth(false)
		return
	}
	s.reportHealth(true)
	blocked := make(map[string]struct{}, len(members))
	for _, member := range members {
		blocked[member] = struct{}{}
	}
	s.mu.Lock()
	s.blocked = blocked
	s.mu.Unlock()
}

func (s *RedisBlacklistStore) publishInvalidate() {
	ctx, cancel := context.WithTimeout(context.Background(), s.opTimeout)
	defer cancel()
	// A lost message only delays propagation; the TTL refresh covers it.
	if err := s.client.Publish(ctx, blacklistInvalidateChan, "1").Err(); err != nil {
		s.logger.Warn("blacklist invalidation publish failed; replicas converge on TTL", "error", err)
	}
}

func (s *RedisBlacklistStore) refreshLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.refresh()
		}
	}
}

// subscribeLoop listens for invalidation messages and reconnects on loss.
// Message loss during a reconnect is covered by the TTL refresh.
func (s *RedisBlacklistStore) subscribeLoop() {
	defer s.wg.Done()
	for {
		if s.subscribe() {
			return
		}
		select {
		case <-s.done:
			return
		case <-time.After(time.Second):
		}
	}
}

// subscribe runs one subscription session; it reports whether the store is
// shutting down (true) or the session ended and should be retried (false).
func (s *RedisBlacklistStore) subscribe() bool {
	sub := s.client.Subscribe(context.Background(), blacklistInvalidateChan)
	defer func() { _ = sub.Close() }()
	messages := sub.Channel()
	for {
		select {
		case <-s.done:
			return true
		case _, ok := <-messages:
			if !ok {
				return false
			}
			s.refresh()
		}
	}
}

func (s *RedisBlacklistStore) reportHealth(up bool) {
	if s.onHealth != nil {
		s.onHealth(up)
	}
}
