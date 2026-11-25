package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// DefaultRateLimitKeyPrefix keeps the historical data-plane namespace.
	// Other limiters (for example login throttling) can use a distinct
	// prefix so their buckets and config never collide with it.
	DefaultRateLimitKeyPrefix = "halon:ratelimit:"
	rateLimitConfigCache      = 10 * time.Second
	rateLimitIdleMargin       = time.Minute
)

// rateLimitScript is a token bucket executed atomically inside Redis. The
// bucket clock is the Redis server clock (TIME), so replicas never disagree
// on refill time. Semantics match golang.org/x/time/rate exactly: tokens
// refill at `rate` per second up to `burst`, one token per request.
var rateLimitScript = redis.NewScript(`
local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local clock = redis.call('TIME')
local now = clock[1] * 1000 + math.floor(clock[2] / 1000)
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local tokens = tonumber(data[1])
local ts = tonumber(data[2])
if tokens == nil or ts == nil then
  tokens = burst
  ts = now
end
tokens = math.min(burst, tokens + (now - ts) * rate / 1000.0)
local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return allowed
`)

// RedisRateLimiter is the shared-mode RateLimitBackend. The token buckets
// live in Redis (one hash per IP), so N replicas together admit exactly the
// configured rate instead of N times it. Limit parameters live in
// halon:ratelimit:config (or the configured prefix's config key) and are
// cached per process for rateLimitConfigCache.
//
// Degradation: when Redis is unreachable, Allow fails open to the embedded
// in-memory RateLimiter (weaker protection beats rejecting everything),
// while config writes fail with ErrRateLimitUnavailable (503).
type RedisRateLimiter struct {
	client    *redis.Client
	logger    *slog.Logger
	onHealth  func(bool)
	opTimeout time.Duration
	fallback  *RateLimiter
	prefix    string

	cfgMu     sync.Mutex
	rps       float64
	burst     int
	cfgExpiry time.Time
}

var _ RateLimitBackend = (*RedisRateLimiter)(nil)

// RedisRateLimiterOption customises a RedisRateLimiter at construction time.
type RedisRateLimiterOption func(*RedisRateLimiter)

// WithRateLimitKeyPrefix overrides the Redis namespace for both bucket keys
// and the shared config key. The prefix is used verbatim, so include the
// trailing colon (for example "halon:ratelimit:login:"). An empty prefix
// keeps DefaultRateLimitKeyPrefix.
func WithRateLimitKeyPrefix(prefix string) RedisRateLimiterOption {
	return func(l *RedisRateLimiter) {
		if strings.TrimSpace(prefix) != "" {
			l.prefix = prefix
		}
	}
}

func NewRedisRateLimiter(client *redis.Client, rps float64, burst int, opTimeout time.Duration, logger *slog.Logger, onHealth func(bool), opts ...RedisRateLimiterOption) *RedisRateLimiter {
	if opTimeout <= 0 {
		opTimeout = 500 * time.Millisecond
	}
	if logger == nil {
		logger = slog.Default()
	}
	limiter := &RedisRateLimiter{
		client: client, logger: logger, onHealth: onHealth, opTimeout: opTimeout,
		fallback: NewRateLimiter(rps, burst),
		rps:      rps, burst: burst,
		prefix: DefaultRateLimitKeyPrefix,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(limiter)
		}
	}
	return limiter
}

// configKey is derived from the bucket prefix so two limiters with different
// prefixes never share their rps/burst settings.
func (l *RedisRateLimiter) configKey() string { return l.prefix + "config" }

func (l *RedisRateLimiter) Allow(ip string) bool {
	rps, burst, err := l.settings()
	if err != nil {
		l.reportHealth(false)
		return l.fallback.Allow(ip)
	}
	ctx, cancel := context.WithTimeout(context.Background(), l.opTimeout)
	defer cancel()
	ttlMs := bucketTTLMs(rps, burst)
	allowed, err := rateLimitScript.Run(ctx, l.client, []string{l.prefix + ip}, rps, burst, ttlMs).Int()
	if err != nil {
		l.reportHealth(false)
		l.logger.Warn("redis rate limit failed; failing open to memory bucket", "error", err)
		return l.fallback.Allow(ip)
	}
	l.reportHealth(true)
	return allowed == 1
}

func (l *RedisRateLimiter) Update(rps float64, burst int) error {
	ctx, cancel := context.WithTimeout(context.Background(), l.opTimeout)
	defer cancel()
	if err := l.client.HSet(ctx, l.configKey(), "rps", rps, "burst", burst).Err(); err != nil {
		l.reportHealth(false)
		return fmt.Errorf("%w: %v", ErrRateLimitUnavailable, err)
	}
	l.reportHealth(true)
	l.cacheSettings(rps, burst)
	return nil
}

// Settings returns the last known parameters; the shared mode tolerates a
// config-cache-aged view for the admin readout.
func (l *RedisRateLimiter) Settings() (float64, int) {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	return l.rps, l.burst
}

// settings resolves the effective parameters, refreshing the short config
// cache from Redis when stale. A missing config key means the seed
// parameters (from env) are still in effect.
func (l *RedisRateLimiter) settings() (float64, int, error) {
	l.cfgMu.Lock()
	if time.Now().Before(l.cfgExpiry) {
		defer l.cfgMu.Unlock()
		return l.rps, l.burst, nil
	}
	l.cfgMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), l.opTimeout)
	defer cancel()
	values, err := l.client.HGetAll(ctx, l.configKey()).Result()
	if err != nil {
		// Keep serving the last known parameters; just extend them briefly
		// so every request does not pay for the outage.
		l.cfgMu.Lock()
		l.cfgExpiry = time.Now().Add(rateLimitConfigCache)
		defer l.cfgMu.Unlock()
		return l.rps, l.burst, err
	}
	rps, rpsErr := strconv.ParseFloat(values["rps"], 64)
	burst, burstErr := strconv.Atoi(values["burst"])
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	if rpsErr == nil && burstErr == nil && rps > 0 && burst > 0 {
		l.rps, l.burst = rps, burst
		_ = l.fallback.Update(rps, burst) // the memory fallback never fails
	}
	l.cfgExpiry = time.Now().Add(rateLimitConfigCache)
	return l.rps, l.burst, nil
}

func (l *RedisRateLimiter) cacheSettings(rps float64, burst int) {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	l.rps, l.burst = rps, burst
	l.cfgExpiry = time.Now().Add(rateLimitConfigCache)
	_ = l.fallback.Update(rps, burst) // the memory fallback never fails
}

func (l *RedisRateLimiter) reportHealth(up bool) {
	if l.onHealth != nil {
		l.onHealth(up)
	}
}

// bucketTTLMs bounds a bucket key's idle lifetime: the time to refill an
// empty bucket plus a margin, mirroring the memory mode's idle eviction.
func bucketTTLMs(rps float64, burst int) int64 {
	refill := float64(burst) / rps
	return int64(math.Ceil(refill*1000)) + rateLimitIdleMargin.Milliseconds()
}
