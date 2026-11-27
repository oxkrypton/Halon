package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/oxkrypton/Halon/internal/incident"
	"github.com/redis/go-redis/v9"
)

func newTestIncidentCoalescer(t *testing.T, mr *miniredis.Miniredis, onHealth func(bool)) *RedisIncidentCoalescer {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisIncidentCoalescer(client, 200*time.Millisecond, testLogger(), onHealth)
}

func defaultImmediatePolicy() IncidentPolicy {
	return IncidentPolicy{
		Enabled:        true,
		MinOccurrences: 1,
		SettleDelay:    15 * time.Second,
		Cooldown:       5 * time.Minute,
		RetryBackoff:   time.Minute,
	}
}

func TestRedisIncidentCoalescerMultiReplicaStormAndSamples(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	replicaA := newTestIncidentCoalescer(t, mr, nil)
	replicaB := newTestIncidentCoalescer(t, mr, nil)
	policy := defaultImmediatePolicy()

	const workers = 40
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		triggered int
		ownerID   string
	)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			c := replicaA
			if idx%2 == 1 {
				c = replicaB
			}
			evt := incident.Event{
				EventID:    fmt.Sprintf("evt-%d", idx),
				Type:       incident.TypeUpstreamTimeout,
				ScopeKey:   "/api/v1/orders/",
				RequestID:  fmt.Sprintf("req-%d", idx),
				OccurredAt: time.Now().UTC(),
				Method:     "GET",
				Path:       fmt.Sprintf("/api/v1/orders/%d", idx%8),
				Route:      "/api/v1/orders/",
				ClientIP:   fmt.Sprintf("10.0.0.%d", idx%7+1),
				StatusCode: 504,
				LatencyMS:  3005,
			}
			res := c.Record(context.Background(), evt, policy)
			mu.Lock()
			if res.Triggered {
				triggered++
				ownerID = res.OwnerEventID
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if triggered != 1 || ownerID == "" {
		t.Fatalf("triggered = %d (owner=%q), want exactly 1", triggered, ownerID)
	}

	snap, ok := replicaB.Snapshot(context.Background(), incident.TypeUpstreamTimeout, "/api/v1/orders/", ownerID)
	if !ok {
		t.Fatal("Snapshot returned false for active window")
	}
	if snap.Occurrences != workers || snap.Status != WindowActive {
		t.Fatalf("snap = %+v, want %d active occurrences", snap, workers)
	}
	if len(snap.SamplePaths) == 0 || len(snap.SamplePaths) > maxCoalesceSamples {
		t.Fatalf("SamplePaths = %v, want 1..%d", snap.SamplePaths, maxCoalesceSamples)
	}
	if len(snap.SampleClientIPs) == 0 || len(snap.SampleClientIPs) > maxCoalesceSamples {
		t.Fatalf("SampleClientIPs = %v, want 1..%d", snap.SampleClientIPs, maxCoalesceSamples)
	}
}

func TestRedisIncidentCoalescerThresholdPromotionAndSubThresholdExpiry(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	replicaA := newTestIncidentCoalescer(t, mr, nil)
	replicaB := newTestIncidentCoalescer(t, mr, nil)

	spikePolicy := IncidentPolicy{
		Enabled:        true,
		MinOccurrences: 5,
		DetectWindow:   30 * time.Second,
		SettleDelay:    10 * time.Second,
		Cooldown:       5 * time.Minute,
		RetryBackoff:   time.Minute,
	}

	// 1..4 hits stay pending and do not pollute active ListWindows().
	for i := 1; i <= 4; i++ {
		c := replicaA
		if i%2 == 0 {
			c = replicaB
		}
		res := c.Record(context.Background(), incident.Event{
			EventID:    fmt.Sprintf("evt-5xx-%d", i),
			Type:       incident.TypeUpstream5xx,
			ScopeKey:   "/api/v1/users/",
			Route:      "/api/v1/users/",
			Path:       fmt.Sprintf("/api/v1/users/%d", i),
			ClientIP:   "10.0.0.1",
			StatusCode: 500,
		}, spikePolicy)
		if res.Triggered || res.Status != WindowPending || res.OwnerEventID != "evt-5xx-1" {
			t.Fatalf("hit %d = %+v, want pending evt-5xx-1", i, res)
		}
	}
	active, err := replicaA.ListWindows(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("pending window must not appear in global active index: %+v", active)
	}
	// Exact point lookup surfaces the pending window.
	exact, err := replicaA.ListWindows(context.Background(), incident.TypeUpstream5xx, "/api/v1/users/")
	if err != nil || len(exact) != 1 || exact[0].Status != WindowPending || exact[0].Occurrences != 4 {
		t.Fatalf("exact pending lookup = (%+v, %v)", exact, err)
	}

	// 5th hit promotes atomically to active with Triggered=true and keeps owner evt-5xx-1.
	hit5 := replicaB.Record(context.Background(), incident.Event{
		EventID:    "evt-5xx-5",
		Type:       incident.TypeUpstream5xx,
		ScopeKey:   "/api/v1/users/",
		Route:      "/api/v1/users/",
		Path:       "/api/v1/users/5",
		ClientIP:   "10.0.0.2",
		StatusCode: 502,
	}, spikePolicy)
	if !hit5.Triggered || hit5.Status != WindowActive || hit5.OwnerEventID != "evt-5xx-1" || hit5.Occurrences != 5 {
		t.Fatalf("5th hit = %+v, want triggered active evt-5xx-1", hit5)
	}
	// 6th hit coalesces into active window.
	hit6 := replicaA.Record(context.Background(), incident.Event{
		EventID:    "evt-5xx-6",
		Type:       incident.TypeUpstream5xx,
		ScopeKey:   "/api/v1/users/",
		Route:      "/api/v1/users/",
		StatusCode: 500,
	}, spikePolicy)
	if hit6.Triggered || hit6.Status != WindowActive || hit6.OwnerEventID != "evt-5xx-1" || hit6.Occurrences != 6 {
		t.Fatalf("6th hit = %+v, want coalesced active evt-5xx-1", hit6)
	}

	active, err = replicaA.ListWindows(context.Background(), incident.TypeUpstream5xx, "")
	if err != nil || len(active) != 1 || active[0].Occurrences != 6 || active[0].Status != WindowActive {
		t.Fatalf("active ListWindows = (%+v, %v)", active, err)
	}

	// Sub-threshold window on another route expires without triggering.
	sub := replicaA.Record(context.Background(), incident.Event{
		EventID:    "evt-sub-1",
		Type:       incident.TypeUpstream5xx,
		ScopeKey:   "/api/v1/orders/",
		Route:      "/api/v1/orders/",
		StatusCode: 500,
	}, spikePolicy)
	if sub.Triggered || sub.Status != WindowPending {
		t.Fatalf("sub-threshold hit = %+v, want pending", sub)
	}
	mr.FastForward(31 * time.Second)
	if afterExp, _ := replicaA.ListWindows(context.Background(), incident.TypeUpstream5xx, "/api/v1/orders/"); len(afterExp) != 0 {
		t.Fatalf("expired pending window should disappear: %+v", afterExp)
	}
}

func TestRedisIncidentCoalescerOwnerGuardAndTypeScopeFilters(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	coalescer := newTestIncidentCoalescer(t, mr, nil)
	policy := defaultImmediatePolicy()

	timeoutEvt := incident.Event{
		EventID: "evt-owner-1", Type: incident.TypeUpstreamTimeout, ScopeKey: "/api/v1/orders/",
		RequestID: "req-1", OccurredAt: time.Now().UTC(),
		Method: "GET", Path: "/api/v1/orders/1", Route: "/api/v1/orders/",
		ClientIP: "10.0.0.1", StatusCode: 504, LatencyMS: 3000,
	}
	abuseEvt := incident.Event{
		EventID: "evt-ip-1", Type: incident.TypeRateLimitAbuse, ScopeKey: "198.51.100.42",
		OccurredAt: time.Now().UTC(), Method: "GET", Path: "/api/v1/orders/1",
		ClientIP: "198.51.100.42", StatusCode: 429,
	}
	if res := coalescer.Record(context.Background(), timeoutEvt, policy); !res.Triggered {
		t.Fatal("expected first Record to open window")
	}
	if res := coalescer.Record(context.Background(), abuseEvt, policy); !res.Triggered {
		t.Fatal("expected IP-scoped abuse Record to open window")
	}

	// Non-owner must not shorten or clear the window.
	coalescer.UpdateCooldown(context.Background(), timeoutEvt.Type, timeoutEvt.ScopeKey, "evt-stale-owner", 0)
	windows, err := coalescer.ListWindows(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ListWindows all: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("ListWindows all = %d windows (%+v), want 2", len(windows), windows)
	}
	ipWindows, err := coalescer.ListWindows(context.Background(), incident.TypeRateLimitAbuse, "")
	if err != nil || len(ipWindows) != 1 || ipWindows[0].ScopeKey != "198.51.100.42" {
		t.Fatalf("filtered IP windows = (%+v, %v)", ipWindows, err)
	}

	// Owner updates cooldown to 1m.
	coalescer.UpdateCooldown(context.Background(), timeoutEvt.Type, timeoutEvt.ScopeKey, "evt-owner-1", time.Minute)
	ttl := mr.TTL(incidentWinKey(timeoutEvt.Type, timeoutEvt.ScopeKey))
	if ttl <= 0 || ttl > time.Minute {
		t.Fatalf("window TTL = %v, want (0, 1m]", ttl)
	}
}

func TestRedisIncidentCoalescerFallsBackToMemoryOnRedisOutage(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	health := true
	coalescer := newTestIncidentCoalescer(t, mr, func(up bool) { health = up })
	mr.Close()

	policy := IncidentPolicy{
		Enabled:        true,
		MinOccurrences: 2,
		DetectWindow:   30 * time.Second,
		SettleDelay:    10 * time.Second,
		Cooldown:       5 * time.Minute,
		RetryBackoff:   time.Minute,
	}
	evt1 := incident.Event{
		EventID: "evt-1", Type: incident.TypeConcurrencyLimit, ScopeKey: "/api/v1/orders/",
		OccurredAt: time.Now().UTC(), Method: "GET", Path: "/api/v1/orders/1", Route: "/api/v1/orders/",
		ClientIP: "10.0.0.1", StatusCode: 429,
	}
	evt2 := evt1
	evt2.EventID = "evt-2"
	evt3 := evt1
	evt3.EventID = "evt-3"

	r1 := coalescer.Record(context.Background(), evt1, policy)
	r2 := coalescer.Record(context.Background(), evt2, policy)
	r3 := coalescer.Record(context.Background(), evt3, policy)
	if r1.Triggered || r1.Status != WindowPending || !r2.Triggered || r2.OwnerEventID != "evt-1" || r3.Triggered || r3.Occurrences != 3 {
		t.Fatalf("fallback threshold coalescing failed: r1=%+v r2=%+v r3=%+v", r1, r2, r3)
	}
	if health {
		t.Fatal("expected health hook to report Redis down")
	}
	windows, _ := coalescer.ListWindows(context.Background(), incident.TypeConcurrencyLimit, "")
	if len(windows) != 1 || windows[0].Occurrences != 3 {
		t.Fatalf("fallback ListWindows = %+v, want 1 window with 3 occurrences", windows)
	}
}

func TestIncidentNotifierThresholdCoalescesFlushesAndAppliesCooldown(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	coalescer := newTestIncidentCoalescer(t, mr, nil)

	received := make(chan incident.Event, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event incident.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		received <- event
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	queue := make(chan incident.Event, 16)
	metrics := NewMetrics()
	policies := IncidentPolicySet{
		Policies: map[incident.EventType]IncidentPolicy{
			incident.TypeUpstream5xx: {
				Enabled:        true,
				MinOccurrences: 3,
				DetectWindow:   30 * time.Second,
				SettleDelay:    10 * time.Second,
				Cooldown:       5 * time.Minute,
				RetryBackoff:   time.Minute,
			},
		},
	}
	notifier := NewIncidentNotifierWithOptions(server.URL, "test-webhook-token", queue, testLogger(), metrics, IncidentNotifierOptions{
		Coalescer: coalescer,
		Policies:  policies,
	})
	done := make(chan struct{})
	go func() {
		notifier.Run()
		close(done)
	}()

	for i := 0; i < 5; i++ {
		queue <- incident.Event{
			EventID: fmt.Sprintf("evt-%d", i), Type: incident.TypeUpstream5xx,
			ScopeKey: "/api/v1/orders/", RequestID: fmt.Sprintf("req-%d", i),
			OccurredAt: time.Now().UTC(), Method: "GET",
			Path: fmt.Sprintf("/api/v1/orders/%d", i), Route: "/api/v1/orders/",
			ClientIP: "10.0.0.5", StatusCode: 500, LatencyMS: 25,
		}
	}
	// Closing the queue must flush the pending settled event immediately.
	close(queue)
	<-done

	select {
	case got := <-received:
		if got.EventID != "evt-0" || got.Type != incident.TypeUpstream5xx || got.Occurrences != 5 || len(got.SamplePaths) != 5 {
			t.Fatalf("unexpected coalesced webhook payload: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected flushed coalesced event")
	}

	ttl := mr.TTL(incidentWinKey(incident.TypeUpstream5xx, "/api/v1/orders/"))
	if ttl <= time.Minute || ttl > 5*time.Minute {
		t.Fatalf("post-delivery cooldown TTL = %v, want (1m, 5m]", ttl)
	}
}
