package gateway

import (
	"crypto/ed25519"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
)

func adminTokenFor(t *testing.T, signer *authz.Signer, role authz.Role) string {
	t.Helper()
	token, err := signer.Issue(authz.IssueOptions{
		Subject:   "admin-" + role.String(),
		Type:      authz.TypeHuman,
		Role:      role,
		Audiences: []authz.Audience{authz.AudienceGatewayAdmin},
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func newAdminSigner(t *testing.T) (*authz.Signer, map[string]ed25519.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	kid := authz.KeyID(public)
	signer, err := authz.NewSigner(private, kid, authz.DefaultIssuer)
	if err != nil {
		t.Fatal(err)
	}
	return signer, map[string]ed25519.PublicKey{kid: public}
}

func newAdminRouter(t *testing.T, mode authz.Mode, keys map[string]ed25519.PublicKey, staticToken string, metrics *Metrics) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	authenticator, err := authz.NewAuthenticator(authz.Config{
		Mode: mode, PublicKeys: keys, Issuer: authz.DefaultIssuer,
	}, authz.AudienceGatewayAdmin, staticToken)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := NewIPRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	limiter := NewRateLimiter(100, 100)
	return NewAdminHandler(staticToken, rules, limiter, testLogger(), metrics, AdminOptions{
		MetricsRequireAuth: true,
		Resilience:         &ResilienceSet{},
		Auth:               authenticator,
	})
}

func adminRequest(router *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestAdminRBACMatrix(t *testing.T) {
	t.Parallel()
	signer, keys := newAdminSigner(t)

	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		allowed map[authz.Role]int
	}{
		{"blacklist read", http.MethodGet, "/admin/rules/blacklist", "", map[authz.Role]int{
			authz.RoleReadOnly: http.StatusOK, authz.RoleOperator: http.StatusOK, authz.RoleFull: http.StatusOK,
		}},
		{"rate limit read", http.MethodGet, "/admin/rules/rate-limit", "", map[authz.Role]int{
			authz.RoleReadOnly: http.StatusOK, authz.RoleOperator: http.StatusOK, authz.RoleFull: http.StatusOK,
		}},
		{"breaker read", http.MethodGet, "/admin/breakers", "", map[authz.Role]int{
			authz.RoleReadOnly: http.StatusOK, authz.RoleOperator: http.StatusOK, authz.RoleFull: http.StatusOK,
		}},
		{"blacklist write", http.MethodPut, "/admin/rules/blacklist/192.0.2.10", "", map[authz.Role]int{
			authz.RoleReadOnly: http.StatusForbidden, authz.RoleOperator: http.StatusCreated, authz.RoleFull: http.StatusCreated,
		}},
		{"rate limit write", http.MethodPut, "/admin/rules/rate-limit", `{"requests_per_second":5,"burst":10}`, map[authz.Role]int{
			authz.RoleReadOnly: http.StatusForbidden, authz.RoleOperator: http.StatusForbidden, authz.RoleFull: http.StatusOK,
		}},
	}
	for _, test := range cases {
		for role, want := range test.allowed {
			t.Run(test.name+"/"+role.String(), func(t *testing.T) {
				router := newAdminRouter(t, authz.ModeJWT, keys, "", nil)
				response := adminRequest(router, test.method, test.path, adminTokenFor(t, signer, role), test.body)
				if response.Code != want {
					t.Fatalf("status = %d, want %d; body = %s", response.Code, want, response.Body.String())
				}
				if want == http.StatusForbidden && !strings.Contains(response.Body.String(), `"FORBIDDEN"`) {
					t.Fatalf("missing FORBIDDEN error body: %s", response.Body.String())
				}
			})
		}
	}
}

func TestAdminAuthenticationModes(t *testing.T) {
	t.Parallel()
	signer, keys := newAdminSigner(t)
	const staticToken = "legacy-admin-token"

	cases := []struct {
		name string
		mode authz.Mode
		body string
		want int
	}{
		{"legacy static", authz.ModeLegacy, staticToken, http.StatusOK},
		{"legacy jwt rejected", authz.ModeLegacy, adminTokenFor(t, signer, authz.RoleFull), http.StatusUnauthorized},
		{"hybrid static", authz.ModeHybrid, staticToken, http.StatusOK},
		{"hybrid jwt", authz.ModeHybrid, adminTokenFor(t, signer, authz.RoleReadOnly), http.StatusOK},
		{"jwt static rejected", authz.ModeJWT, staticToken, http.StatusUnauthorized},
		{"jwt token", authz.ModeJWT, adminTokenFor(t, signer, authz.RoleReadOnly), http.StatusOK},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			router := newAdminRouter(t, test.mode, keys, staticToken, nil)
			response := adminRequest(router, http.MethodGet, "/admin/rules/blacklist", test.body, "")
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.want, response.Body.String())
			}
			if test.want == http.StatusUnauthorized && !strings.Contains(response.Body.String(), `"UNAUTHORIZED"`) {
				t.Fatalf("missing UNAUTHORIZED error body: %s", response.Body.String())
			}
		})
	}

	router := newAdminRouter(t, authz.ModeJWT, keys, "", nil)
	response := adminRequest(router, http.MethodGet, "/admin/rules/blacklist", "", "")
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("missing token: status = %d, header = %q", response.Code, response.Header().Get("WWW-Authenticate"))
	}
}

func TestMetricsEndpointAuthzAndNoAudit(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	signer, keys := newAdminSigner(t)
	metrics := NewMetrics()
	rules, err := NewIPRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	var auditLog strings.Builder
	logger := slog.New(slog.NewTextHandler(&auditLog, nil))
	authenticator, err := authz.NewAuthenticator(authz.Config{
		Mode: authz.ModeJWT, PublicKeys: keys, Issuer: authz.DefaultIssuer,
	}, authz.AudienceGatewayAdmin, "")
	if err != nil {
		t.Fatal(err)
	}
	router := NewAdminHandler("", rules, NewRateLimiter(100, 100), logger, metrics, AdminOptions{
		MetricsRequireAuth: true,
		Auth:               authenticator,
	})

	if response := adminRequest(router, http.MethodGet, "/metrics", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", response.Code)
	}
	readonlyToken := adminTokenFor(t, signer, authz.RoleReadOnly)
	if response := adminRequest(router, http.MethodGet, "/metrics", readonlyToken, ""); response.Code != http.StatusOK {
		t.Fatalf("readonly token: status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if audit := auditLog.String(); strings.Contains(audit, "admin request") {
		t.Fatalf("metrics scrape wrote an audit entry: %s", audit)
	}
	// The audit middleware is also the only writer of the admin request
	// counter; a scrape must not add a gateway:metrics:read sample.
	response := adminRequest(router, http.MethodGet, "/metrics", readonlyToken, "")
	if strings.Contains(response.Body.String(), `permission="gateway:metrics:read"`) {
		t.Fatal("metrics scrape incremented the admin request counter")
	}
}

func TestAdminAuditMetric(t *testing.T) {
	t.Parallel()
	signer, keys := newAdminSigner(t)
	metrics := NewMetrics()
	router := newAdminRouter(t, authz.ModeJWT, keys, "", metrics)
	token := adminTokenFor(t, signer, authz.RoleReadOnly)

	for i := 0; i < 2; i++ {
		if response := adminRequest(router, http.MethodGet, "/admin/rules/blacklist", token, ""); response.Code != http.StatusOK {
			t.Fatalf("status = %d", response.Code)
		}
	}
	if response := adminRequest(router, http.MethodPut, "/admin/rules/rate-limit", token, `{"requests_per_second":5,"burst":10}`); response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
	response := adminRequest(router, http.MethodGet, "/metrics", token, "")
	body := response.Body.String()
	for _, expected := range []string{
		"halon_gateway_admin_requests_total",
		`role="readonly"`,
		`permission="gateway:rules:read"`,
		`result="deny"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics body missing %q: %s", expected, body)
		}
	}
}
