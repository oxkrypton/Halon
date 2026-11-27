package gateway

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/incident"
	"github.com/redis/go-redis/v9"
)

func TestAdminStateEndpointsAndRBAC(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	metrics := NewMetrics()
	rules := NewRedisBlacklistStore(client, time.Minute, 200*time.Millisecond, testLogger(), metrics.SetRedisUp)
	t.Cleanup(rules.Close)
	limiter := NewRedisRateLimiter(client, 15, 30, 200*time.Millisecond, testLogger(), metrics.SetRedisUp)
	coalescer := NewRedisIncidentCoalescer(client, 200*time.Millisecond, testLogger(), metrics.SetRedisUp)

	if _, err := rules.Add("203.0.113.10"); err != nil {
		t.Fatal(err)
	}
	if !limiter.Allow("203.0.113.10") {
		t.Fatal("expected initial Allow to succeed")
	}
	coalescer.Record(context.Background(), incident.Event{
		EventID: "evt-state-1", Type: incident.TypeUpstreamTimeout, ScopeKey: "/api/v1/orders/",
		RequestID: "req-1", OccurredAt: time.Now().UTC(),
		Method: "GET", Path: "/api/v1/orders/7", Route: "/api/v1/orders/",
		ClientIP: "203.0.113.10", StatusCode: 504, LatencyMS: 3005,
	}, IncidentPolicy{Enabled: true, MinOccurrences: 1, SettleDelay: 15 * time.Second, Cooldown: 5 * time.Minute})
	coalescer.Record(context.Background(), incident.Event{
		EventID: "evt-state-2", Type: incident.TypeRateLimitAbuse, ScopeKey: "203.0.113.10",
		OccurredAt: time.Now().UTC(), Method: "GET", Path: "/api/v1/orders/7",
		ClientIP: "203.0.113.10", StatusCode: 429,
	}, IncidentPolicy{Enabled: true, MinOccurrences: 1, SettleDelay: 10 * time.Second, Cooldown: 10 * time.Minute})

	routes, err := ParseRoutes("/api/v1/orders/=http://orders:8092,/api/v1/users/=http://users:8090")
	if err != nil {
		t.Fatal(err)
	}
	resilience := NewResilienceSet(routes.Prefixes(), ResilienceConfig{
		BreakerEnabled: true, BreakerFailureThreshold: 5, BreakerOpenDuration: 30 * time.Second, BreakerHalfOpenSuccesses: 1,
		Metrics: metrics, Logger: testLogger(),
	}, func(string) http.RoundTripper { return http.DefaultTransport })

	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	kid := authz.KeyID(public)
	signer, err := authz.NewSigner(private, kid, authz.DefaultIssuer)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := authz.NewAuthenticator(authz.Config{
		Mode: authz.ModeJWT, PublicKeys: map[string]ed25519.PublicKey{kid: public}, Issuer: authz.DefaultIssuer,
	}, authz.AudienceGatewayAdmin, "")
	if err != nil {
		t.Fatal(err)
	}

	readOnlyToken, err := signer.Issue(authz.IssueOptions{
		Subject: "agent-ro", Type: authz.TypeAgent, Role: authz.RoleReadOnly,
		Audiences: []authz.Audience{authz.AudienceGatewayAdmin}, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	pcrToken, err := signer.Issue(authz.IssueOptions{
		Subject: "human-pcr", Type: authz.TypeHuman, Role: authz.RoleFull, PasswordChangeRequired: true,
		Audiences: []authz.Audience{authz.AudienceGatewayAdmin}, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	router := NewAdminHandler(testAdminToken, rules, limiter, testLogger(), metrics, AdminOptions{
		Auth:              authenticator,
		Resilience:        resilience,
		Routes:            routes,
		IncidentCoalescer: coalescer,
	})

	doGet := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Auth checks.
	for _, path := range []string{"/admin/state/gateway", "/admin/state/rate-limit/203.0.113.10", "/admin/state/incident-windows"} {
		if got := doGet(path, "").Code; got != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s = %d, want 401", path, got)
		}
		if got := doGet(path, pcrToken).Code; got != http.StatusForbidden {
			t.Fatalf("pcr %s = %d, want 403", path, got)
		}
	}

	// 1) /admin/state/gateway
	gwResp := doGet("/admin/state/gateway", readOnlyToken)
	if gwResp.Code != http.StatusOK {
		t.Fatalf("gateway state = %d %s", gwResp.Code, gwResp.Body.String())
	}
	var gwBody struct {
		RedisUp   bool `json:"redis_up"`
		Blacklist struct {
			Count int      `json:"count"`
			IPs   []string `json:"ips"`
		} `json:"blacklist"`
		RateLimit struct {
			RPS   float64 `json:"requests_per_second"`
			Burst int     `json:"burst"`
		} `json:"rate_limit"`
		Breakers []BreakerStatus `json:"breakers"`
	}
	if err := json.Unmarshal(gwResp.Body.Bytes(), &gwBody); err != nil {
		t.Fatal(err)
	}
	if !gwBody.RedisUp || gwBody.Blacklist.Count != 1 || gwBody.RateLimit.Burst != 30 || len(gwBody.Breakers) != 2 {
		t.Fatalf("unexpected gateway state: %+v", gwBody)
	}

	// 2) /admin/state/rate-limit/:ip (active, inactive, invalid)
	ipResp := doGet("/admin/state/rate-limit/203.0.113.10", readOnlyToken)
	if ipResp.Code != http.StatusOK {
		t.Fatalf("ip state = %d %s", ipResp.Code, ipResp.Body.String())
	}
	var ipBody struct {
		IP          string  `json:"ip"`
		InBlacklist bool    `json:"in_blacklist"`
		Active      bool    `json:"active"`
		Tokens      float64 `json:"tokens"`
		Burst       int     `json:"burst"`
	}
	if err := json.Unmarshal(ipResp.Body.Bytes(), &ipBody); err != nil {
		t.Fatal(err)
	}
	if !ipBody.InBlacklist || !ipBody.Active || ipBody.Tokens >= 30 || ipBody.Burst != 30 {
		t.Fatalf("unexpected active ip state: %+v", ipBody)
	}

	idleResp := doGet("/admin/state/rate-limit/198.51.100.1", readOnlyToken)
	if idleResp.Code != http.StatusOK {
		t.Fatalf("idle ip state = %d %s", idleResp.Code, idleResp.Body.String())
	}
	if err := json.Unmarshal(idleResp.Body.Bytes(), &ipBody); err != nil {
		t.Fatal(err)
	}
	if ipBody.InBlacklist || ipBody.Active || ipBody.Tokens != 30 {
		t.Fatalf("unexpected idle ip state: %+v", ipBody)
	}

	if bad := doGet("/admin/state/rate-limit/not-an-ip", readOnlyToken); bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid ip = %d, want 400", bad.Code)
	}

	// 3) /admin/state/incident-windows
	winResp := doGet("/admin/state/incident-windows?type=upstream_timeout&scope=%2Fapi%2Fv1%2Forders%2F", readOnlyToken)
	if winResp.Code != http.StatusOK {
		t.Fatalf("windows state = %d %s", winResp.Code, winResp.Body.String())
	}
	var winBody struct {
		Windows []IncidentWindowSnapshot `json:"windows"`
	}
	if err := json.Unmarshal(winResp.Body.Bytes(), &winBody); err != nil {
		t.Fatal(err)
	}
	if len(winBody.Windows) != 1 || winBody.Windows[0].EventID != "evt-state-1" || winBody.Windows[0].EventType != incident.TypeUpstreamTimeout {
		t.Fatalf("unexpected windows body: %+v", winBody)
	}
	if badType := doGet("/admin/state/incident-windows?type=BadType!", readOnlyToken); badType.Code != http.StatusBadRequest {
		t.Fatalf("invalid type param = %d, want 400", badType.Code)
	}

	// Redis down -> /admin/state/rate-limit/:ip returns 503.
	mr.Close()
	if down := doGet("/admin/state/rate-limit/203.0.113.10", readOnlyToken); down.Code != http.StatusServiceUnavailable {
		t.Fatalf("redis down ip state = %d, want 503", down.Code)
	}
}
