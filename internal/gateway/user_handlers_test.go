package gateway

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/identity"
)

type stubUserService struct {
	users         []identity.UserView
	total         int64
	user          *identity.UserView
	createResult  *identity.CreateUserResult
	updateResult  *identity.UserView
	resetPassword string
	err           error
}

func (s *stubUserService) ListUsers(context.Context, int, int) ([]identity.UserView, int64, error) {
	return s.users, s.total, s.err
}

func (s *stubUserService) GetUser(context.Context, string) (*identity.UserView, error) {
	return s.user, s.err
}

func (s *stubUserService) CreateUser(context.Context, identity.CreateUserInput) (*identity.CreateUserResult, error) {
	return s.createResult, s.err
}

func (s *stubUserService) UpdateUser(context.Context, string, string, string, identity.UserUpdate, string, string) (*identity.UserView, error) {
	return s.updateResult, s.err
}

func (s *stubUserService) ResetPassword(context.Context, string, string, string, string) (string, error) {
	return s.resetPassword, s.err
}

const testUserID = "11111111-1111-4111-8111-111111111111"

func sampleUserView() identity.UserView {
	return identity.UserView{
		ID: testUserID, Username: "alice", DisplayName: "Alice",
		Role: authz.RoleFull, MustChangePassword: false,
	}
}

func newUserRouter(t *testing.T, keys map[string]ed25519.PublicKey, service UserService) *gin.Engine {
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
	return NewAdminHandler("", rules, NewRateLimiter(100, 100), testLogger(), nil, AdminOptions{
		Auth: authenticator, LoginEnabled: true,
		AuthService: &stubAuthService{}, UserService: service,
	})
}

func userEndpoints() []struct {
	Method, Path, Body string
	Success            int
} {
	return []struct {
		Method, Path, Body string
		Success            int
	}{
		{http.MethodGet, "/admin/users", "", http.StatusOK},
		{http.MethodGet, "/admin/users/" + testUserID, "", http.StatusOK},
		{http.MethodPost, "/admin/users", `{"username":"bob","role":"readonly"}`, http.StatusCreated},
		{http.MethodPatch, "/admin/users/" + testUserID, `{"role":"operator"}`, http.StatusOK},
		{http.MethodPost, "/admin/users/" + testUserID + "/password", ``, http.StatusOK},
	}
}

func TestUserEndpointsRoleMatrix(t *testing.T) {
	t.Parallel()
	signer, keys := newAdminSigner(t)
	service := &stubUserService{
		users: []identity.UserView{sampleUserView()}, total: 1,
		user:          &[]identity.UserView{sampleUserView()}[0],
		createResult:  &identity.CreateUserResult{User: sampleUserView()},
		updateResult:  &[]identity.UserView{sampleUserView()}[0],
		resetPassword: "generated-once",
	}
	router := newUserRouter(t, keys, service)

	for _, endpoint := range userEndpoints() {
		for _, test := range []struct {
			role authz.Role
			want int
		}{
			{authz.RoleReadOnly, http.StatusForbidden},
			{authz.RoleOperator, http.StatusForbidden},
			{authz.RoleFull, endpoint.Success},
		} {
			response := adminRequest(router, endpoint.Method, endpoint.Path,
				adminTokenFor(t, signer, test.role), endpoint.Body)
			if response.Code != test.want {
				t.Fatalf("%s %s role=%s: status=%d want=%d body=%s",
					endpoint.Method, endpoint.Path, test.role, response.Code, test.want, response.Body.String())
			}
		}
		if response := adminRequest(router, endpoint.Method, endpoint.Path, "", endpoint.Body); response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token: status=%d want 401", endpoint.Method, endpoint.Path, response.Code)
		}
	}
}

func TestUserEndpointsRejectPCRToken(t *testing.T) {
	t.Parallel()
	signer, keys := newAdminSigner(t)
	token, err := signer.Issue(authz.IssueOptions{
		Subject: "alice", Type: authz.TypeHuman, Role: authz.RoleFull,
		Audiences: []authz.Audience{authz.AudienceGatewayAdmin},
		UserID:    "user-1", PasswordChangeRequired: true,
		TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &stubUserService{
		users: []identity.UserView{sampleUserView()}, total: 1,
		user:         &[]identity.UserView{sampleUserView()}[0],
		createResult: &identity.CreateUserResult{User: sampleUserView()},
		updateResult: &[]identity.UserView{sampleUserView()}[0],
	}
	router := newUserRouter(t, keys, service)

	for _, endpoint := range userEndpoints() {
		response := adminRequest(router, endpoint.Method, endpoint.Path, token, endpoint.Body)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"PASSWORD_CHANGE_REQUIRED"`) {
			t.Fatalf("%s %s pcr token: status=%d body=%s",
				endpoint.Method, endpoint.Path, response.Code, response.Body.String())
		}
	}
}

func TestUserResponsesNeverIncludePasswordHash(t *testing.T) {
	t.Parallel()
	view := sampleUserView()
	service := &stubUserService{
		users: []identity.UserView{view}, total: 1, user: &view,
		createResult: &identity.CreateUserResult{User: view},
		updateResult: &view,
	}
	router := newUserRouter(t, nil, service)
	signer, _ := newAdminSigner(t)
	token := adminTokenFor(t, signer, authz.RoleFull)

	for _, endpoint := range []struct{ Method, Path, Body string }{
		{http.MethodGet, "/admin/users", ""},
		{http.MethodGet, "/admin/users/" + testUserID, ""},
		{http.MethodPost, "/admin/users", `{"username":"bob","role":"readonly","password":"supplied"}`},
		{http.MethodPatch, "/admin/users/" + testUserID, `{"display_name":"Bob"}`},
	} {
		response := adminRequest(router, endpoint.Method, endpoint.Path, token, endpoint.Body)
		if strings.Contains(strings.ToLower(response.Body.String()), "password_hash") {
			t.Fatalf("%s leaked password hash: %s", endpoint.Path, response.Body.String())
		}
	}
}

func TestCreateUserReturnsGeneratedPasswordOnce(t *testing.T) {
	t.Parallel()
	signer, keys := newAdminSigner(t)
	view := sampleUserView()
	view.MustChangePassword = true
	service := &stubUserService{
		createResult: &identity.CreateUserResult{User: view, GeneratedPassword: "one-time-secret"},
	}
	router := newUserRouter(t, keys, service)
	token := adminTokenFor(t, signer, authz.RoleFull)

	response := adminRequest(router, http.MethodPost, "/admin/users", token,
		`{"username":"bob","role":"readonly"}`)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"generated_password":"one-time-secret"`) {
		t.Fatalf("generated password response: status=%d body=%s", response.Code, response.Body.String())
	}
	service.createResult = &identity.CreateUserResult{User: sampleUserView()}
	response = adminRequest(router, http.MethodPost, "/admin/users", token,
		`{"username":"bob","role":"readonly","password":"supplied"}`)
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), "generated_password") {
		t.Fatalf("supplied password response: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUserManagementErrorMapping(t *testing.T) {
	t.Parallel()
	signer, keys := newAdminSigner(t)
	token := adminTokenFor(t, signer, authz.RoleFull)
	cases := []struct {
		name, method, path, body string
		err                      error
		want                     int
		code                     string
	}{
		{"username taken", http.MethodPost, "/admin/users", `{"username":"bob","role":"readonly"}`, identity.ErrUsernameTaken, http.StatusConflict, "USERNAME_TAKEN"},
		{"weak password", http.MethodPost, "/admin/users", `{"username":"bob","role":"readonly","password":"short"}`, identity.ErrWeakPassword, http.StatusBadRequest, "WEAK_PASSWORD"},
		{"weak reset password", http.MethodPost, "/admin/users/" + testUserID + "/password", ``, identity.ErrWeakPassword, http.StatusBadRequest, "WEAK_PASSWORD"},
		{"last admin", http.MethodPatch, "/admin/users/" + testUserID, `{"role":"readonly"}`, identity.ErrLastAdmin, http.StatusConflict, "LAST_ADMIN"},
		{"self modification", http.MethodPatch, "/admin/users/" + testUserID, `{"role":"readonly"}`, identity.ErrCannotModifySelf, http.StatusForbidden, "FORBIDDEN"},
		{"not found", http.MethodGet, "/admin/users/" + testUserID, "", identity.ErrNotFound, http.StatusNotFound, "USER_NOT_FOUND"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			service := &stubUserService{err: test.err}
			router := newUserRouter(t, keys, service)
			response := adminRequest(router, test.method, test.path, token, test.body)
			if response.Code != test.want || !strings.Contains(response.Body.String(), `"`+test.code+`"`) {
				t.Fatalf("status=%d body=%s; want %d %s", response.Code, response.Body.String(), test.want, test.code)
			}
		})
	}
}

func TestUserRoutesDisabledByDefault(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	rules, err := NewIPRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	router := NewAdminHandler("", rules, NewRateLimiter(100, 100), testLogger(), nil, AdminOptions{})
	for _, endpoint := range userEndpoints() {
		response := adminRequest(router, endpoint.Method, endpoint.Path, "", endpoint.Body)
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"LOGIN_DISABLED"`) {
			t.Fatalf("%s %s: status=%d body=%s", endpoint.Method, endpoint.Path, response.Code, response.Body.String())
		}
	}
}

func TestUserInputValidationRejectsBadUUIDAndLongDisplayName(t *testing.T) {
	t.Parallel()
	signer, keys := newAdminSigner(t)
	token := adminTokenFor(t, signer, authz.RoleFull)
	router := newUserRouter(t, keys, &stubUserService{})

	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/admin/users/not-a-uuid", ""},
		{http.MethodPatch, "/admin/users/not-a-uuid", `{"role":"operator"}`},
		{http.MethodPost, "/admin/users/not-a-uuid/password", ""},
		{http.MethodPost, "/admin/users", `{"username":"bob","role":"readonly","display_name":"` + strings.Repeat("a", 129) + `"}`},
	} {
		resp := adminRequest(router, tc.method, tc.path, token, tc.body)
		if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), `"INVALID_REQUEST"`) {
			t.Fatalf("%s %s: status=%d body=%s", tc.method, tc.path, resp.Code, resp.Body.String())
		}
	}
}
