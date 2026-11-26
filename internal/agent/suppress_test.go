package agent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedisRouteSuppressor(t *testing.T, mr *miniredis.Miniredis, onHealth func(bool)) *RedisRouteSuppressor {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisRouteSuppressor(client, 200*time.Millisecond, testLogger(), onHealth)
}

func TestRedisRouteSuppressorLifecycle(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	store := newTestRedisRouteSuppressor(t, mr, nil)

	acquired, owner, err := store.TryAcquire(context.Background(), "/api/v1/orders/", "evt-1", 5*time.Minute)
	if err != nil || !acquired || owner != "evt-1" {
		t.Fatalf("first TryAcquire = (%v, %q, %v)", acquired, owner, err)
	}
	acquired, owner, err = store.TryAcquire(context.Background(), "/api/v1/orders/", "evt-2", 5*time.Minute)
	if err != nil || acquired || owner != "evt-1" {
		t.Fatalf("second TryAcquire = (%v, %q, %v), want (false, evt-1, nil)", acquired, owner, err)
	}

	// Wrong owner cannot modify or delete the window.
	_ = store.SetTTL(context.Background(), "/api/v1/orders/", "evt-2", time.Second)
	_ = store.Release(context.Background(), "/api/v1/orders/", "evt-2")
	mr.FastForward(10 * time.Second)
	if acquired, _, _ := store.TryAcquire(context.Background(), "/api/v1/orders/", "evt-3", 5*time.Minute); acquired {
		t.Fatal("wrong owner modified redis window")
	}

	// Owner shortens TTL to 30s (backoff) and waits past it.
	_ = store.SetTTL(context.Background(), "/api/v1/orders/", "evt-1", 30*time.Second)
	mr.FastForward(35 * time.Second)
	acquired, owner, err = store.TryAcquire(context.Background(), "/api/v1/orders/", "evt-4", 5*time.Minute)
	if err != nil || !acquired || owner != "evt-4" {
		t.Fatalf("TryAcquire after backoff = (%v, %q, %v), want (true, evt-4, nil)", acquired, owner, err)
	}

	// Owner release clears immediately.
	_ = store.Release(context.Background(), "/api/v1/orders/", "evt-4")
	acquired, owner, err = store.TryAcquire(context.Background(), "/api/v1/orders/", "evt-5", 5*time.Minute)
	if err != nil || !acquired || owner != "evt-5" {
		t.Fatalf("TryAcquire after release = (%v, %q, %v), want (true, evt-5, nil)", acquired, owner, err)
	}
}

func TestRedisRouteSuppressorMultiReplicaConcurrency(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	replicaA := newTestRedisRouteSuppressor(t, mr, nil)
	replicaB := newTestRedisRouteSuppressor(t, mr, nil)

	const total = 40
	var acquiredCount atomic.Int32
	owners := make([]string, total)
	var winner atomic.Value

	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		i := i
		store := replicaA
		if i%2 == 1 {
			store = replicaB
		}
		go func() {
			defer wg.Done()
			eventID := fmt.Sprintf("evt-%d", i)
			acquired, owner, err := store.TryAcquire(context.Background(), "/api/v1/orders/", eventID, 5*time.Minute)
			if err != nil {
				t.Errorf("TryAcquire(%s): %v", eventID, err)
				return
			}
			owners[i] = owner
			if acquired {
				acquiredCount.Add(1)
				winner.Store(eventID)
			}
		}()
	}
	wg.Wait()

	if got := acquiredCount.Load(); got != 1 {
		t.Fatalf("acquiredCount = %d, want exactly 1", got)
	}
	wantOwner, _ := winner.Load().(string)
	for i, owner := range owners {
		if owner != wantOwner {
			t.Fatalf("caller %d observed owner %q, want %q", i, owner, wantOwner)
		}
	}
}

func TestRedisRouteSuppressorFailsOpen(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	var up atomic.Bool
	up.Store(true)
	store := newTestRedisRouteSuppressor(t, mr, func(healthy bool) { up.Store(healthy) })
	mr.Close()

	acquired, owner, err := store.TryAcquire(context.Background(), "/api/v1/orders/", "evt-1", 5*time.Minute)
	if err != nil || !acquired || owner != "evt-1" {
		t.Fatalf("first degraded TryAcquire = (%v, %q, %v), want fail-open acquire", acquired, owner, err)
	}
	if up.Load() {
		t.Fatal("health hook was not notified of redis outage")
	}
	// When Redis is unreachable, subsequent events fail open too rather than suppressing.
	acquired, owner, err = store.TryAcquire(context.Background(), "/api/v1/orders/", "evt-2", 5*time.Minute)
	if err != nil || !acquired || owner != "evt-2" {
		t.Fatalf("second degraded TryAcquire = (%v, %q, %v), want fail-open acquire", acquired, owner, err)
	}
	if err := store.SetTTL(context.Background(), "/api/v1/orders/", "evt-1", time.Minute); err != nil {
		t.Fatalf("degraded SetTTL: %v", err)
	}
	if err := store.Release(context.Background(), "/api/v1/orders/", "evt-1"); err != nil {
		t.Fatalf("degraded Release: %v", err)
	}
}
