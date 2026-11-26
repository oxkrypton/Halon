package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedisCooldownStore(t *testing.T, mr *miniredis.Miniredis, onHealth func(bool)) *RedisCooldownStore {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisCooldownStore(client, 200*time.Millisecond, slog.Default(), onHealth)
	t.Cleanup(store.Close)
	return store
}

func TestRedisCooldownStoreClaimAndExpiry(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	store := newTestRedisCooldownStore(t, mr, nil)

	if _, claimed, err := store.Claim(context.Background(), "deployment/default/web", 5*time.Minute); err != nil || !claimed {
		t.Fatalf("first Claim() = _, %v, %v", claimed, err)
	}
	remaining, claimed, err := store.Claim(context.Background(), "deployment/default/web", 5*time.Minute)
	if err != nil || claimed {
		t.Fatalf("second Claim() = %v, %v, %v", remaining, claimed, err)
	}
	if remaining <= 0 || remaining > 5*time.Minute {
		t.Fatalf("remaining = %v, want within (0, 5m]", remaining)
	}
	// After the window the next claim succeeds again.
	mr.FastForward(6 * time.Minute)
	if _, claimed, err := store.Claim(context.Background(), "deployment/default/web", 5*time.Minute); err != nil || !claimed {
		t.Fatalf("Claim() after expiry = _, %v, %v", claimed, err)
	}
}

// TestRedisCooldownStoreMultiReplica guarantees two agent replicas cannot both
// mutate the same resource within the cooldown window.
func TestRedisCooldownStoreMultiReplica(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	replicaA := newTestRedisCooldownStore(t, mr, nil)
	replicaB := newTestRedisCooldownStore(t, mr, nil)

	claims := 0
	for _, store := range []*RedisCooldownStore{replicaA, replicaB} {
		if _, claimed, err := store.Claim(context.Background(), "pod/default/pod-1", time.Minute); err != nil {
			t.Fatal(err)
		} else if claimed {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("%d replicas claimed the same action, want exactly 1", claims)
	}
}

func TestRedisCooldownStoreDegradesClosed(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	health := []bool{true}
	store := newTestRedisCooldownStore(t, mr, func(up bool) { health[0] = up })
	mr.Close()

	if _, _, err := store.Claim(context.Background(), "deployment/default/web", time.Minute); !errors.Is(err, ErrCooldownUnavailable) {
		t.Fatalf("Claim() with redis down = %v, want ErrCooldownUnavailable", err)
	}
	if health[0] {
		t.Fatal("health hook was not notified of the outage")
	}
}

func TestKubernetesToolsetSharedCooldown(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	options := func() KubernetesOptions {
		return KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
			Cooldowns: newTestRedisCooldownStore(t, mr, nil),
		}
	}
	toolsetA, _ := NewKubernetesTools(nil, options())
	toolsetB, _ := NewKubernetesTools(nil, options())

	if err := toolsetA.allowAction(context.Background(), "pod/default/pod-1"); err != nil {
		t.Fatal(err)
	}
	err := toolsetB.allowAction(context.Background(), "pod/default/pod-1")
	if err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("second replica allowAction() = %v, want a cooldown error", err)
	}
	if !strings.Contains(err.Error(), "retry after") {
		t.Fatalf("cooldown error lacks the remaining time: %v", err)
	}
}

func TestKubernetesToolsetRejectsNilCooldownStore(t *testing.T) {
	t.Parallel()
	toolset, _ := NewKubernetesTools(nil, KubernetesOptions{AllowedNamespaces: []string{"default"}})
	if err := toolset.allowAction(context.Background(), "deployment/default/web"); !errors.Is(err, ErrCooldownUnavailable) {
		t.Fatalf("allowAction() with nil Cooldowns = %v, want ErrCooldownUnavailable", err)
	}
}
