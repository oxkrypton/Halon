package gateway

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedisRateLimiter(t *testing.T, mr *miniredis.Miniredis, rps float64, burst int, onHealth func(bool), opts ...RedisRateLimiterOption) *RedisRateLimiter {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisRateLimiter(client, rps, burst, 200*time.Millisecond, slog.Default(), onHealth, opts...)
}

func TestRedisRateLimiterDefaultKeyPrefix(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	limiter := newTestRedisRateLimiter(t, mr, 1, 1, nil)

	if !limiter.Allow("192.0.2.10") {
		t.Fatal("Allow() = false with redis up")
	}
	if !mr.Exists("halon:ratelimit:192.0.2.10") {
		t.Fatalf("default bucket key missing; keys = %v", mr.Keys())
	}
	if err := limiter.Update(3, 4); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists("halon:ratelimit:config") {
		t.Fatalf("default config key missing; keys = %v", mr.Keys())
	}
}

func TestRedisRateLimiterCustomKeyPrefix(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	const prefix = "halon:ratelimit:login:"
	limiter := newTestRedisRateLimiter(t, mr, 1, 1, nil, WithRateLimitKeyPrefix(prefix))

	if !limiter.Allow("192.0.2.11") {
		t.Fatal("Allow() = false with redis up")
	}
	if !mr.Exists(prefix + "192.0.2.11") {
		t.Fatalf("custom bucket key missing; keys = %v", mr.Keys())
	}
	if mr.Exists("halon:ratelimit:192.0.2.11") {
		t.Error("custom prefix wrote into the default namespace")
	}
	if err := limiter.Update(3, 4); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists(prefix + "config") {
		t.Fatalf("custom config key missing; keys = %v", mr.Keys())
	}
	if mr.Exists("halon:ratelimit:config") {
		t.Error("custom prefix wrote the default config key")
	}
}

func TestRedisRateLimiterPrefixesAreIsolated(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	const (
		prefixA = "halon:ratelimit:data:"
		prefixB = "halon:ratelimit:login:"
	)
	data := newTestRedisRateLimiter(t, mr, 1, 1, nil, WithRateLimitKeyPrefix(prefixA))
	login := newTestRedisRateLimiter(t, mr, 1, 1, nil, WithRateLimitKeyPrefix(prefixB))

	if !data.Allow("192.0.2.12") || data.Allow("192.0.2.12") {
		t.Fatal("data-plane bucket did not exhaust at burst 1")
	}
	// The login limiter has its own bucket for the same client IP.
	if !login.Allow("192.0.2.12") {
		t.Fatal("login bucket was throttled by the data-plane bucket")
	}
	if login.Allow("192.0.2.12") {
		t.Fatal("login bucket did not exhaust at its own burst 1")
	}

	// Config writes are isolated too.
	if err := data.Update(9, 9); err != nil {
		t.Fatal(err)
	}
	if _, _, err := login.settings(); err != nil {
		t.Fatal(err)
	}
	if rps, burst := login.Settings(); rps != 1 || burst != 1 {
		t.Fatalf("login settings = %v/%v, want the seeded 1/1", rps, burst)
	}
}

func TestRedisRateLimiterTokenBucketSemantics(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	limiter := newTestRedisRateLimiter(t, mr, 2, 2, nil)

	// The bucket clock is the Redis server clock; pin it for determinism.
	start := time.Now()
	mr.SetTime(start)

	first, second := limiter.Allow("192.0.2.1"), limiter.Allow("192.0.2.1")
	if !first || !second {
		t.Fatal("burst of 2 was not admitted")
	}
	if limiter.Allow("192.0.2.1") {
		t.Fatal("third request beyond burst was admitted")
	}
	// 0.6s at 2 rps refills 1.2 tokens: exactly one more request may pass.
	mr.SetTime(start.Add(600 * time.Millisecond))
	if !limiter.Allow("192.0.2.1") {
		t.Fatal("refilled token was not admitted")
	}
	if limiter.Allow("192.0.2.1") {
		t.Fatal("partial token was admitted")
	}
	// Buckets are per IP.
	if !limiter.Allow("192.0.2.2") {
		t.Fatal("a different IP was throttled by the first IP's bucket")
	}
}

// TestRedisRateLimiterMultiReplica is the key shared-mode guarantee: two
// replicas together admit the configured rate, not twice it.
func TestRedisRateLimiterMultiReplica(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	limiterA := newTestRedisRateLimiter(t, mr, 1, 2, nil)
	limiterB := newTestRedisRateLimiter(t, mr, 1, 2, nil)

	allowed := 0
	for range 4 {
		if limiterA.Allow("192.0.2.9") {
			allowed++
		}
		if limiterB.Allow("192.0.2.9") {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("two replicas admitted %d requests, want the shared burst of 2", allowed)
	}
}

func TestRedisRateLimiterConfigSharedAcrossReplicas(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	writer := newTestRedisRateLimiter(t, mr, 1, 1, nil)
	if err := writer.Update(5, 7); err != nil {
		t.Fatal(err)
	}
	// A fresh replica picks the shared config up on its first request.
	reader := newTestRedisRateLimiter(t, mr, 1, 1, nil)
	if _, _, err := reader.settings(); err != nil {
		t.Fatal(err)
	}
	if rps, burst := reader.Settings(); rps != 5 || burst != 7 {
		t.Fatalf("reader settings = %v/%v, want 5/7", rps, burst)
	}
	// The stale writer still applies the new config after its cache expires.
	writer.cfgMu.Lock()
	writer.cfgExpiry = time.Time{}
	writer.cfgMu.Unlock()
	if _, _, err := writer.settings(); err != nil {
		t.Fatal(err)
	}
	if rps, burst := writer.Settings(); rps != 5 || burst != 7 {
		t.Fatalf("writer settings after cache expiry = %v/%v, want 5/7", rps, burst)
	}
}

func TestRedisRateLimiterDegradesWhenRedisDown(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	health := []bool{true}
	limiter := newTestRedisRateLimiter(t, mr, 2, 2, func(up bool) { health[0] = up })

	if !limiter.Allow("192.0.2.5") {
		t.Fatal("Allow() = false with redis up")
	}
	mr.Close()

	// Fail-open: requests are admitted even beyond the configured burst while Redis is unreachable.
	for i := range 5 {
		if !limiter.Allow("192.0.2.5") {
			t.Fatalf("Allow() call %d did not fail open with redis down", i)
		}
	}
	// Config writes fail loudly instead of pretending success.
	if err := limiter.Update(5, 5); !errors.Is(err, ErrRateLimitUnavailable) {
		t.Fatalf("Update() with redis down = %v, want ErrRateLimitUnavailable", err)
	}
	if health[0] {
		t.Fatal("health hook was not notified of the outage")
	}
}
