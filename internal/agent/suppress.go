package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const routeSuppressKeyPrefix = "halon:incident:route:"

// RouteSuppressor arbitrates per-route suppression windows so a timeout storm
// on one route triggers at most one investigation per window.
//
// Each open window is owned by one event ID. SetTTL and Release only mutate a
// window when it is still owned by that event ID, preventing a delayed finish
// from overwriting or deleting a newer investigation's window.
type RouteSuppressor interface {
	// TryAcquire reserves route for owner for ttl. It returns acquired=true
	// when this call opens the window, or acquired=false with currentOwner
	// when another window is already active.
	TryAcquire(ctx context.Context, route, owner string, ttl time.Duration) (acquired bool, currentOwner string, err error)
	// SetTTL updates the remaining lifetime of route if still owned by owner.
	// A non-positive ttl releases the window immediately.
	SetTTL(ctx context.Context, route, owner string, ttl time.Duration) error
	// Release deletes the window for route if still owned by owner.
	Release(ctx context.Context, route, owner string) error
}

type memoryWindow struct {
	owner     string
	expiresAt time.Time
}

// MemoryRouteSuppressor is the single-replica RouteSuppressor backed by an
// in-memory map. It is safe for concurrent use.
type MemoryRouteSuppressor struct {
	mu      sync.Mutex
	clock   func() time.Time
	windows map[string]memoryWindow
}

var _ RouteSuppressor = (*MemoryRouteSuppressor)(nil)

func NewMemoryRouteSuppressor() *MemoryRouteSuppressor {
	return &MemoryRouteSuppressor{
		clock:   time.Now,
		windows: make(map[string]memoryWindow),
	}
}

func (m *MemoryRouteSuppressor) TryAcquire(_ context.Context, route, owner string, ttl time.Duration) (bool, string, error) {
	if ttl <= 0 {
		return true, owner, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	if w, ok := m.windows[route]; ok && now.Before(w.expiresAt) {
		return false, w.owner, nil
	}
	m.windows[route] = memoryWindow{owner: owner, expiresAt: now.Add(ttl)}
	m.evictLocked(now)
	return true, owner, nil
}

func (m *MemoryRouteSuppressor) SetTTL(_ context.Context, route, owner string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.windows[route]
	if !ok || w.owner != owner {
		return nil
	}
	if ttl <= 0 {
		delete(m.windows, route)
		return nil
	}
	w.expiresAt = m.clock().Add(ttl)
	m.windows[route] = w
	return nil
}

func (m *MemoryRouteSuppressor) Release(_ context.Context, route, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.windows[route]; ok && w.owner == owner {
		delete(m.windows, route)
	}
	return nil
}

func (m *MemoryRouteSuppressor) evictLocked(now time.Time) {
	if len(m.windows) < 1024 && len(m.windows)&(len(m.windows)-1) != 0 {
		return
	}
	for route, w := range m.windows {
		if !now.Before(w.expiresAt) {
			delete(m.windows, route)
		}
	}
}

// acquireWindowScript atomically acquires a route suppression window when no
// key exists, returning {1, owner}. Otherwise it returns {0, currentOwner}.
var acquireWindowScript = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then
  return {1, ARGV[1]}
end
return {0, redis.call('GET', KEYS[1]) or ''}
`)

// setTTLIfOwnerScript atomically updates the key's TTL only when the stored
// value matches the caller's owner token (event ID).
var setTTLIfOwnerScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

// releaseIfOwnerScript atomically deletes the key only when the stored value
// matches the caller's owner token (event ID).
var releaseIfOwnerScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// RedisRouteSuppressor is the shared-mode RouteSuppressor. Windows live in
// Redis under halon:incident:route:<route> so N replicas together trigger at
// most one investigation per route window.
//
// Degradation: unlike mutating K8s cooldowns (which fail closed for safety),
// storm suppression is a noise/cost optimization. When Redis is unreachable it
// fails open to an embedded MemoryRouteSuppressor so incidents are still
// investigated (at worst one per replica per window).
type RedisRouteSuppressor struct {
	client    *redis.Client
	opTimeout time.Duration
	logger    *slog.Logger
	onHealth  func(bool)
	fallback  *MemoryRouteSuppressor
}

var _ RouteSuppressor = (*RedisRouteSuppressor)(nil)

func NewRedisRouteSuppressor(client *redis.Client, opTimeout time.Duration, logger *slog.Logger, onHealth func(bool)) *RedisRouteSuppressor {
	if opTimeout <= 0 {
		opTimeout = 500 * time.Millisecond
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RedisRouteSuppressor{
		client:    client,
		opTimeout: opTimeout,
		logger:    logger,
		onHealth:  onHealth,
		fallback:  NewMemoryRouteSuppressor(),
	}
}

func (s *RedisRouteSuppressor) TryAcquire(ctx context.Context, route, owner string, ttl time.Duration) (bool, string, error) {
	if ttl <= 0 {
		return true, owner, nil
	}
	opCtx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
	raw, err := acquireWindowScript.Run(opCtx, s.client, []string{routeSuppressKeyPrefix + route}, owner, ttlMillis(ttl)).Slice()
	if err != nil {
		s.reportHealth(false)
		s.logger.Warn("redis route suppression acquire failed; failing open to memory", "route", route, "error", err)
		return s.fallback.TryAcquire(ctx, route, owner, ttl)
	}
	s.reportHealth(true)
	acquired, currentOwner, parseErr := parseAcquireResult(raw)
	if parseErr != nil {
		s.logger.Warn("unexpected redis route suppression response; failing open to memory", "route", route, "error", parseErr)
		return s.fallback.TryAcquire(ctx, route, owner, ttl)
	}
	return acquired, currentOwner, nil
}

func (s *RedisRouteSuppressor) SetTTL(ctx context.Context, route, owner string, ttl time.Duration) error {
	_ = s.fallback.SetTTL(ctx, route, owner, ttl)
	if ttl <= 0 {
		return s.Release(ctx, route, owner)
	}
	opCtx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
	if err := setTTLIfOwnerScript.Run(opCtx, s.client, []string{routeSuppressKeyPrefix + route}, owner, ttlMillis(ttl)).Err(); err != nil {
		s.reportHealth(false)
		s.logger.Warn("redis route suppression ttl update failed", "route", route, "error", err)
		return nil
	}
	s.reportHealth(true)
	return nil
}

func (s *RedisRouteSuppressor) Release(ctx context.Context, route, owner string) error {
	_ = s.fallback.Release(ctx, route, owner)
	opCtx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
	if err := releaseIfOwnerScript.Run(opCtx, s.client, []string{routeSuppressKeyPrefix + route}, owner).Err(); err != nil {
		s.reportHealth(false)
		s.logger.Warn("redis route suppression release failed", "route", route, "error", err)
		return nil
	}
	s.reportHealth(true)
	return nil
}

func (s *RedisRouteSuppressor) reportHealth(up bool) {
	if s.onHealth != nil {
		s.onHealth(up)
	}
}

func ttlMillis(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms < 1 {
		return 1
	}
	return ms
}

func parseAcquireResult(raw []any) (bool, string, error) {
	if len(raw) != 2 {
		return false, "", fmt.Errorf("expected 2 elements, got %d", len(raw))
	}
	flag, ok := raw[0].(int64)
	if !ok {
		return false, "", fmt.Errorf("unexpected acquired flag type %T", raw[0])
	}
	owner, ok := raw[1].(string)
	if !ok {
		return false, "", fmt.Errorf("unexpected owner type %T", raw[1])
	}
	return flag == 1, owner, nil
}
