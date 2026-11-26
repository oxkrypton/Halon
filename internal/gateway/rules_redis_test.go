package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedisBlacklistStore(t *testing.T, mr *miniredis.Miniredis, ttl time.Duration) *RedisBlacklistStore {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisBlacklistStore(client, ttl, 200*time.Millisecond, slog.Default(), nil)
	t.Cleanup(store.Close)
	return store
}

// waitForSubscribers blocks until count subscriptions are registered on the
// invalidation channel, so test publishes are never lost to a subscription
// that has not completed yet (production covers that loss with the TTL).
func waitForSubscribers(t *testing.T, mr *miniredis.Miniredis, count int64) {
	t.Helper()
	probe := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = probe.Close() })
	waitForCondition(t, func() bool {
		receivers, err := probe.Publish(t.Context(), blacklistInvalidateChan, "probe").Result()
		return err == nil && receivers >= count
	}, "pub/sub subscriptions were not registered")
}

func waitForCondition(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", message)
}

func TestRedisBlacklistStoreAddContainsRemove(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	store := newTestRedisBlacklistStore(t, mr, time.Minute)

	created, err := store.Add("192.0.2.10")
	if err != nil || !created {
		t.Fatalf("Add() = %v, %v", created, err)
	}
	if !store.Contains("192.0.2.10") {
		t.Fatal("Contains() = false after Add()")
	}
	if members, err := mr.Members(blacklistSetKey); err != nil || len(members) != 1 || members[0] != "192.0.2.10" {
		t.Fatalf("redis set members = %v, %v", members, err)
	}
	created, err = store.Add("192.0.2.10")
	if err != nil || created {
		t.Fatalf("duplicate Add() = %v, %v", created, err)
	}
	removed, err := store.Remove("192.0.2.10")
	if err != nil || !removed {
		t.Fatalf("Remove() = %v, %v", removed, err)
	}
	if store.Contains("192.0.2.10") {
		t.Fatal("Contains() = true after Remove()")
	}
	removed, err = store.Remove("192.0.2.10")
	if err != nil || removed {
		t.Fatalf("duplicate Remove() = %v, %v", removed, err)
	}
	if _, err := store.Add("not-an-ip"); err == nil {
		t.Fatal("Add() accepted invalid IP")
	}
	created, err = store.Add("2001:0db8::1")
	if err != nil || !created {
		t.Fatalf("Add(IPv6) = %v, %v", created, err)
	}
	if !store.Contains("2001:db8::1") {
		t.Fatal("canonical IPv6 address was not matched")
	}
	created, err = store.Add("2001:db8::1")
	if err != nil || created {
		t.Fatalf("duplicate IPv6 Add() = %v, %v", created, err)
	}
}

func TestRedisBlacklistStoreRefreshPicksUpExternalWrites(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	store := newTestRedisBlacklistStore(t, mr, time.Minute)

	if _, err := mr.SAdd(blacklistSetKey, "198.51.100.7"); err != nil {
		t.Fatal(err)
	}
	store.refresh()
	if !store.Contains("198.51.100.7") {
		t.Fatal("refresh() did not pick up the external write")
	}
	if _, err := mr.SRem(blacklistSetKey, "198.51.100.7"); err != nil {
		t.Fatal(err)
	}
	store.refresh()
	if store.Contains("198.51.100.7") {
		t.Fatal("refresh() did not drop the externally removed entry")
	}
}

// TestRedisBlacklistStoreMultiReplica is the key shared-mode guarantee: a
// write through one replica becomes visible on another replica that only
// reads its cache — via pub/sub immediately, and via TTL at worst.
func TestRedisBlacklistStoreMultiReplica(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	writer := newTestRedisBlacklistStore(t, mr, time.Hour) // TTL long: only pub/sub may propagate
	reader := newTestRedisBlacklistStore(t, mr, time.Hour)
	waitForSubscribers(t, mr, 2)

	if _, err := writer.Add("203.0.113.66"); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, func() bool { return reader.Contains("203.0.113.66") },
		"pub/sub invalidation did not propagate the new entry to the second replica")

	if _, err := writer.Remove("203.0.113.66"); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, func() bool { return !reader.Contains("203.0.113.66") },
		"pub/sub invalidation did not propagate the removal to the second replica")
}

func TestRedisBlacklistStoreTTLFallbackCoversMessageLoss(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	reader := newTestRedisBlacklistStore(t, mr, 30*time.Millisecond)

	// Write directly to Redis, bypassing this replica's publish path.
	if _, err := mr.SAdd(blacklistSetKey, "203.0.113.99"); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, func() bool { return reader.Contains("203.0.113.99") },
		"TTL refresh did not cover the missed invalidation")
}

func TestAdminBlacklistSharedModeResponses(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	store := newTestRedisBlacklistStore(t, mr, 10*time.Second)
	admin := NewAdminHandler(testAdminToken, store, newTestRedisRateLimiter(t, mr, 1, 1, nil), testLogger(), nil, AdminOptions{})

	request := httptest.NewRequest(http.MethodPut, "/admin/rules/blacklist/192.0.2.30", nil)
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	response := httptest.NewRecorder()
	admin.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("PUT blacklist = %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "propagation") {
		t.Fatalf("shared-mode write response lacks propagation note: %s", response.Body.String())
	}

	mr.Close()
	response = httptest.NewRecorder()
	admin.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT blacklist with redis down = %d, want 503", response.Code)
	}
	if !strings.Contains(response.Body.String(), "BLACKLIST_UNAVAILABLE") {
		t.Fatalf("503 body lacks BLACKLIST_UNAVAILABLE: %s", response.Body.String())
	}
}

func TestRedisBlacklistStoreDegradesWhenRedisDown(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	health := []bool{true}
	store := NewRedisBlacklistStore(redis.NewClient(&redis.Options{Addr: mr.Addr()}),
		time.Hour, 100*time.Millisecond, slog.Default(), func(up bool) { health[0] = up })
	t.Cleanup(store.Close)

	if _, err := store.Add("192.0.2.20"); err != nil {
		t.Fatal(err)
	}
	mr.Close()

	// Reads keep serving the last known cache.
	if !store.Contains("192.0.2.20") {
		t.Fatal("degraded Contains() lost the cached entry")
	}
	// Writes fail loudly instead of pretending success.
	if _, err := store.Add("192.0.2.21"); !errors.Is(err, ErrBlacklistUnavailable) {
		t.Fatalf("Add() with redis down = %v, want ErrBlacklistUnavailable", err)
	}
	if _, err := store.Remove("192.0.2.20"); !errors.Is(err, ErrBlacklistUnavailable) {
		t.Fatalf("Remove() with redis down = %v, want ErrBlacklistUnavailable", err)
	}
	if health[0] {
		t.Fatal("health hook was not notified of the outage")
	}
}
