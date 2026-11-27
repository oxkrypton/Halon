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
// on refill time. Tokens refill at `rate` per second up to `burst`, one token
// per request.
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

// RedisRateLimiter backs RateLimitBackend with Redis Lua token buckets (one
// hash per IP), so all gateway replicas together admit the configured rate.
// Limit parameters live in halon:ratelimit:config (or the configured prefix's
// config key) and are cached per process for rateLimitConfigCache.
//
// Degradation: when Redis is unreachable, Allow fails open (admitting traffic
// rather than blocking the data plane), while config writes fail with
// ErrRateLimitUnavailable (503).
type RedisRateLimiter struct {
	client    *redis.Client
	logger    *slog.Logger
	onHealth  func(bool)
	opTimeout time.Duration
	prefix    string

	cfgMu     sync.Mutex
	rps       float64
	burst     int
	cfgExpiry time.Time
}

var _ RateLimitBackend = (*RedisRateLimiter)(nil)
var _ RateLimitInspector = (*RedisRateLimiter)(nil)

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
		rps: rps, burst: burst,
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
		l.logger.Warn("redis rate limit config refresh failed; failing open", "error", err)
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), l.opTimeout)
	defer cancel()
	ttlMs := bucketTTLMs(rps, burst)
	allowed, err := rateLimitScript.Run(ctx, l.client, []string{l.prefix + ip}, rps, burst, ttlMs).Int()
	if err != nil {
		l.reportHealth(false)
		l.logger.Warn("redis rate limit failed; failing open", "error", err)
		return true
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

// Settings returns the last known parameters; callers tolerate a
// config-cache-aged view for the admin readout.
func (l *RedisRateLimiter) Settings() (float64, int) {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	return l.rps, l.burst
}

// InspectIP reads the current Redis token-bucket state for ip without
// consuming a token.
func (l *RedisRateLimiter) InspectIP(ctx context.Context, ip string) (RateLimitIPStatus, error) {
	canonical, err := canonicalIP(ip)
	if err != nil {
		return RateLimitIPStatus{}, err
	}
	rps, burst, err := l.settings()
	if err != nil {
		l.reportHealth(false)
		return RateLimitIPStatus{}, fmt.Errorf("%w: %v", ErrRateLimitUnavailable, err)
	}
	opCtx, cancel := context.WithTimeout(ctx, l.opTimeout)
	defer cancel()

	key := l.prefix + canonical
	pipe := l.client.Pipeline()
	hmgetCmd := pipe.HMGet(opCtx, key, "tokens", "ts")
	pttlCmd := pipe.PTTL(opCtx, key)
	timeCmd := pipe.Time(opCtx)
	if _, err := pipe.Exec(opCtx); err != nil && err != redis.Nil {
		l.reportHealth(false)
		return RateLimitIPStatus{}, fmt.Errorf("%w: %v", ErrRateLimitUnavailable, err)
	}
	l.reportHealth(true)

	status := RateLimitIPStatus{
		IP:                canonical,
		Active:            false,
		Tokens:            float64(burst),
		RequestsPerSecond: rps,
		Burst:             burst,
	}
	values := hmgetCmd.Val()
	if len(values) != 2 || values[0] == nil || values[1] == nil {
		return status, nil
	}
	tokens, tokensErr := strconv.ParseFloat(fmt.Sprint(values[0]), 64)
	ts, tsErr := strconv.ParseInt(fmt.Sprint(values[1]), 10, 64)
	if tokensErr != nil || tsErr != nil {
		return status, nil
	}
	nowTime, timeErr := timeCmd.Result()
	nowMs := time.Now().UTC().UnixMilli()
	if timeErr == nil {
		nowMs = nowTime.UnixMilli()
	}
	refilled := tokens + float64(nowMs-ts)*rps/1000.0
	if refilled > float64(burst) {
		refilled = float64(burst)
	}
	if refilled < 0 {
		refilled = 0
	}
	ttlMS := int64(0)
	if pttl, ttlErr := pttlCmd.Result(); ttlErr == nil && pttl > 0 {
		ttlMS = pttl.Milliseconds()
	}
	status.Active = true
	status.Tokens = math.Round(refilled*1000) / 1000
	status.TTLMS = ttlMS
	return status, nil
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
	}
	l.cfgExpiry = time.Now().Add(rateLimitConfigCache)
	return l.rps, l.burst, nil
}

func (l *RedisRateLimiter) cacheSettings(rps float64, burst int) {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	l.rps, l.burst = rps, burst
	l.cfgExpiry = time.Now().Add(rateLimitConfigCache)
}

func (l *RedisRateLimiter) reportHealth(up bool) {
	if l.onHealth != nil {
		l.onHealth(up)
	}
}

// bucketTTLMs bounds a bucket key's idle lifetime: the time to refill an
// empty bucket plus a margin.
func bucketTTLMs(rps float64, burst int) int64 {
	refill := float64(burst) / rps
	return int64(math.Ceil(refill*1000)) + rateLimitIdleMargin.Milliseconds()
}
