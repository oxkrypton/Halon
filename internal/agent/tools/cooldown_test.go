package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type failingCooldownStore struct{}

func (failingCooldownStore) Claim(context.Context, string, time.Duration) (time.Duration, bool, error) {
	return 0, false, ErrCooldownUnavailable
}

func TestMemoryCooldownStoreClaimExpiryAndConcurrency(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	store := NewMemoryCooldownStoreWithClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})

	if _, claimed, err := store.Claim(context.Background(), "deployment/default/web", 5*time.Minute); err != nil || !claimed {
		t.Fatalf("first Claim() = _, %v, %v", claimed, err)
	}
	remaining, claimed, err := store.Claim(context.Background(), "deployment/default/web", 5*time.Minute)
	if err != nil || claimed {
		t.Fatalf("second Claim() = %v, %v, %v", remaining, claimed, err)
	}
	if remaining != 5*time.Minute {
		t.Fatalf("remaining = %v, want 5m", remaining)
	}

	mu.Lock()
	now = now.Add(6 * time.Minute)
	mu.Unlock()
	if _, claimed, err := store.Claim(context.Background(), "deployment/default/web", 5*time.Minute); err != nil || !claimed {
		t.Fatalf("Claim() after expiry = _, %v, %v", claimed, err)
	}

	// Concurrent claims on one fresh key must admit exactly one caller.
	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(20)
	for i := 0; i < 20; i++ {
		go func() {
			defer wg.Done()
			if _, ok, err := store.Claim(context.Background(), "pod/default/api-1", time.Minute); err == nil && ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("concurrent Claim wins = %d, want 1", wins.Load())
	}
}

func TestResourceLockerSerializesSameKey(t *testing.T) {
	t.Parallel()
	locker := NewResourceLocker()
	var active atomic.Int32
	var maxObserved atomic.Int32
	var wg sync.WaitGroup

	wg.Add(10)
	for i := 0; i < 10; i++ {
		go func() {
			defer wg.Done()
			unlock := locker.Lock("deployment/default/orders")
			defer unlock()
			cur := active.Add(1)
			for {
				prev := maxObserved.Load()
				if cur <= prev || maxObserved.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			active.Add(-1)
		}()
	}
	wg.Wait()

	if maxObserved.Load() != 1 {
		t.Fatalf("max concurrent holders for same key = %d, want 1", maxObserved.Load())
	}
	locker.mu.Lock()
	leaked := len(locker.entries)
	locker.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("locker leaked %d map entries after unlocks", leaked)
	}
}

func TestKubernetesToolsetCooldownFormattingAndUnavailable(t *testing.T) {
	t.Parallel()
	toolset, _ := NewKubernetesTools(nil, KubernetesOptions{
		AllowedNamespaces: []string{"default"},
		ActionCooldown:    time.Minute,
	})
	if err := toolset.allowAction(context.Background(), "pod/default/pod-1"); err != nil {
		t.Fatal(err)
	}
	err := toolset.allowAction(context.Background(), "pod/default/pod-1")
	if err == nil || !strings.Contains(err.Error(), "cooling down") || !strings.Contains(err.Error(), "retry after") {
		t.Fatalf("allowAction() = %v, want cooldown error with retry after", err)
	}

	failingToolset, _ := NewKubernetesTools(nil, KubernetesOptions{
		AllowedNamespaces: []string{"default"},
		Cooldowns:         failingCooldownStore{},
	})
	if err := failingToolset.allowAction(context.Background(), "deployment/default/web"); !errors.Is(err, ErrCooldownUnavailable) {
		t.Fatalf("allowAction() with failing store = %v, want ErrCooldownUnavailable", err)
	}
}
