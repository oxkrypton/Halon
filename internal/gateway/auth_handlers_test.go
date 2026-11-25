package gateway

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/identity"
)

type stubAuthService struct {
	session    *identity.Session
	loginErr   error
	refreshErr error
	logoutErr  error
	changeErr  error
	loginCalls int
}

func (s *stubAuthService) Login(context.Context, string, string, string, string, string) (*identity.Session, error) {
	s.loginCalls++
	return s.session, s.loginErr
}

func (s *stubAuthService) Refresh(context.Context, string, string, string, string) (*identity.Session, error) {
	return s.session, s.refreshErr
}

func (s *stubAuthService) Logout(context.Context, string, string, string) error { return s.logoutErr }

func (s *stubAuthService) ChangePassword(context.Context, string, string, string, string, string) error {
	return s.changeErr
}

// #nosec G101 -- fixed fixture value used only to assert JSON shape in tests.
func sampleSession() *identity.Session {
	return &identity.Session{
		AccessToken:        "signed-access-token",
		ExpiresIn:          900,
		RefreshToken:       "opaque-refresh-token",
		MustChangePassword: true,
		User: identity.SessionUser{
			ID: "user-1", Username: "alice", Role: authz.RoleOperator,
		},
	}
}

func newAuthRouter(t *testing.T, keys map[string]ed25519.PublicKey, service AuthService, limiter RateLimitBackend, logger *slog.Logger) *gin.Engine {
	t.Helper()
	if len(keys) == 0 {
		_, keys = newAdminSigner(t)
	}
	gin.SetMode(gin.TestMode)
	authenticator, err := authz.NewAuthenticator(authz.Config{
		Mode: authz.ModeJWT, PublicKeys: keys, Issuer: authz.DefaultIssuer,
	}, authz.AudienceGatewayAdmin, "")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := NewIPRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	return NewAdminHandler("", rules, NewRateLimiter(100, 100), logger, nil, AdminOptions{
		Auth: authenticator, LoginEnabled: true, AuthService: service, LoginLimiter: limiter,
	})
}

func TestAuthRoutesDisabledByDefault(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	rules, err := NewIPRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	router := NewAdminHandler("", rules, NewRateLimiter(100, 100), testLogger(), nil, AdminOptions{})
	for _, test := range []struct{ method, path string }{
		{http.MethodPost, "/auth/login"},
		{http.MethodPost, "/auth/refresh"},
		{http.MethodPost, "/auth/logout"},
		{http.MethodGet, "/auth/me"},
		{http.MethodPost, "/auth/password"},
	} {
		response := adminRequest(router, test.method, test.path, "", `{}`)
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"LOGIN_DISABLED"`) {
			t.Fatalf("%s %s: status=%d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
	}
}

func TestLoginEndpointSuccessAndUniformFailures(t *testing.T) {
	t.Parallel()
	service := &stubAuthService{session: sampleSession()}
	router := newAuthRouter(t, nil, service, nil, testLogger())

	response := adminRequest(router, http.MethodPost, "/auth/login", "",
		`{"username":"alice","password":"correct-horse-battery-staple"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, expected := range []string{
		`"access_token":"signed-access-token"`, `"expires_in":900`,
		`"refresh_token":"opaque-refresh-token"`, `"must_change_password":true`,
		`"id":"user-1"`, `"username":"alice"`, `"role":"operator"`,
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("login body missing %s: %s", expected, response.Body.String())
		}
	}

	service.loginErr = identity.ErrInvalidCredentials
	var firstError errorDetail
	for i, body := range []string{
		`{"username":"nobody","password":"whatever"}`,
		`{"username":"alice","password":"wrong"}`,
	} {
		response := adminRequest(router, http.MethodPost, "/auth/login", "", body)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.Code)
		}
		var envelope errorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		// request_id is intentionally unique per request; code and message
		// must be identical so callers cannot enumerate usernames.
		if i == 0 {
			firstError = envelope.Error
		} else if envelope.Error.Code != firstError.Code || envelope.Error.Message != firstError.Message {
			t.Fatalf("failure details differ: %+v vs %+v", firstError, envelope.Error)
		}
	}
	if firstError.Code != "INVALID_CREDENTIALS" || firstError.Message != "invalid username or password" {
		t.Fatalf("unexpected failure detail: %+v", firstError)
	}
}

func TestLoginEndpointRateLimited(t *testing.T) {
	t.Parallel()
	service := &stubAuthService{session: sampleSession()}
	limiter := NewRateLimiter(0.001, 1)
	router := newAuthRouter(t, nil, service, limiter, testLogger())
	body := `{"username":"alice","password":"correct-horse-battery-staple"}`

	if response := adminRequest(router, http.MethodPost, "/auth/login", "", body); response.Code != http.StatusOK {
		t.Fatalf("first login status = %d", response.Code)
	}
	response := adminRequest(router, http.MethodPost, "/auth/login", "", body)
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), `"RATE_LIMITED"`) {
		t.Fatalf("rate limited login: status=%d body=%s", response.Code, response.Body.String())
	}
	if service.loginCalls != 1 {
		t.Fatalf("service called %d times, want 1", service.loginCalls)
	}
}

func TestRefreshAndLogoutEndpoints(t *testing.T) {
	t.Parallel()
	service := &stubAuthService{session: sampleSession()}
	router := newAuthRouter(t, nil, service, nil, testLogger())

	response := adminRequest(router, http.MethodPost, "/auth/refresh", "", `{"refresh_token":"old"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"refresh_token":"opaque-refresh-token"`) {
		t.Fatalf("refresh: status=%d body=%s", response.Code, response.Body.String())
	}
	service.refreshErr = identity.ErrInvalidRefreshToken
	response = adminRequest(router, http.MethodPost, "/auth/refresh", "", `{"refresh_token":"bad"}`)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"INVALID_REFRESH_TOKEN"`) {
		t.Fatalf("invalid refresh: status=%d body=%s", response.Code, response.Body.String())
	}
	response = adminRequest(router, http.MethodPost, "/auth/logout", "", `{"refresh_token":"old"}`)
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", response.Code)
	}
	service.logoutErr = errors.New("database down")
	response = adminRequest(router, http.MethodPost, "/auth/logout", "", `{"refresh_token":"old"}`)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"INTERNAL_ERROR"`) {
		t.Fatalf("logout failure: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMeAndPasswordEndpointsAcceptPCRToken(t *testing.T) {
	t.Parallel()
	signer, keys := newAdminSigner(t)
	token, err := signer.Issue(authz.IssueOptions{
		Subject: "alice", Type: authz.TypeHuman, Role: authz.RoleFull,
		Audiences: []authz.Audience{authz.AudienceGatewayAdmin},
		UserID:    "user-1", PasswordChangeRequired: true, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &stubAuthService{}
	router := newAuthRouter(t, keys, service, nil, testLogger())

	response := adminRequest(router, http.MethodGet, "/auth/me", token, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"must_change_password":true`) {
		t.Fatalf("me with pcr token: status=%d body=%s", response.Code, response.Body.String())
	}
	response = adminRequest(router, http.MethodPost, "/auth/password", token,
		`{"current_password":"old","new_password":"new-secret"}`)
	if response.Code != http.StatusNoContent {
		t.Fatalf("password change with pcr token: status=%d body=%s", response.Code, response.Body.String())
	}
	service.changeErr = identity.ErrWeakPassword
	response = adminRequest(router, http.MethodPost, "/auth/password", token,
		`{"current_password":"old","new_password":"short"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"WEAK_PASSWORD"`) {
		t.Fatalf("weak password: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := adminRequest(router, http.MethodGet, "/auth/me", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("me without token: status = %d", response.Code)
	}
}

func TestAuthLogsNeverContainSecrets(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	service := &stubAuthService{session: sampleSession()}
	router := newAuthRouter(t, nil, service, nil, logger)

	adminRequest(router, http.MethodPost, "/auth/login", "",
		`{"username":"alice","password":"super-secret-password-1234"}`)
	output := logs.String()
	for _, secret := range []string{"super-secret-password-1234", "signed-access-token", "opaque-refresh-token"} {
		if strings.Contains(output, secret) {
			t.Fatalf("auth log leaked %q: %s", secret, output)
		}
	}
}
