package authz

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

func TestRolePermissionMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		permission Permission
		readonly   bool
		operator   bool
		full       bool
	}{
		{PermIncidentRead, true, true, true},
		{PermGatewayRulesRead, true, true, true},
		{PermGatewayBreakerRead, true, true, true},
		{PermGatewayMetricsRead, true, true, true},
		{PermToolObserve, true, true, true},
		{PermGatewayBlacklistWrite, false, true, true},
		{PermToolK8sRestartPod, false, true, true},
		{PermToolK8sScale, false, true, true},
		{PermGatewayRateLimitWrite, false, false, true},
		{PermToolK8sUpdateImage, false, false, true},
	}
	for _, test := range cases {
		if got := RoleReadOnly.Allows(test.permission); got != test.readonly {
			t.Errorf("readonly %s = %v, want %v", test.permission, got, test.readonly)
		}
		if got := RoleOperator.Allows(test.permission); got != test.operator {
			t.Errorf("operator %s = %v, want %v", test.permission, got, test.operator)
		}
		if got := RoleFull.Allows(test.permission); got != test.full {
			t.Errorf("full %s = %v, want %v", test.permission, got, test.full)
		}
	}
	if _, err := ParseRole("viewer"); err == nil {
		t.Error("expected unknown role to fail")
	}
	if _, err := ParsePermission("gateway:*:write"); err == nil {
		t.Error("expected unknown permission to fail")
	}
}

type jwtFixture struct {
	signer   *Signer
	verifier *Verifier
	key      ed25519.PublicKey
	private  ed25519.PrivateKey
	kid      string
}

func newJWTFixture(t *testing.T, revoked ...string) jwtFixture {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	kid := KeyID(public)
	signer, err := NewSigner(private, kid, DefaultIssuer)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{kid: public}, DefaultIssuer, ParseRevokedJTIs(strings.Join(revoked, ",")))
	if err != nil {
		t.Fatal(err)
	}
	return jwtFixture{signer: signer, verifier: verifier, key: public, private: private, kid: kid}
}

func validOptions() IssueOptions {
	return IssueOptions{
		Subject:  "alice",
		Type:     TypeHuman,
		Role:     RoleOperator,
		Audience: AudienceGatewayAdmin,
		TTL:      time.Hour,
		JTI:      "jti-1",
	}
}

func TestJWTVerify(t *testing.T) {
	t.Parallel()
	fixture := newJWTFixture(t)

	raw, err := fixture.signer.Issue(validOptions())
	if err != nil {
		t.Fatal(err)
	}
	principal, err := fixture.verifier.Verify(raw, AudienceGatewayAdmin)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if principal.Subject != "alice" || principal.Type != TypeHuman || principal.Role != RoleOperator || principal.JTI != "jti-1" {
		t.Fatalf("unexpected principal: %+v", principal)
	}
	if _, err := fixture.verifier.Verify(raw, AudienceAgent); err == nil {
		t.Error("cross-audience token was accepted")
	}

	// Generated jti values must be present and distinct.
	first, _ := fixture.signer.Issue(validOptions().withGeneratedJTI())
	second, _ := fixture.signer.Issue(validOptions().withGeneratedJTI())
	firstPrincipal, err := fixture.verifier.Verify(first, AudienceGatewayAdmin)
	if err != nil || firstPrincipal.JTI == "" {
		t.Fatalf("generated jti missing: %+v %v", firstPrincipal, err)
	}
	if firstPrincipal.JTI == second {
		t.Error("jti generation is not random")
	}
}

func (o IssueOptions) withGeneratedJTI() IssueOptions {
	o.JTI = ""
	return o
}

func TestVerifierRejectsInvalidTokens(t *testing.T) {
	t.Parallel()
	fixture := newJWTFixture(t, "revoked-1")

	sign := func(claims *Claims, kid string, method jwt.SigningMethod, key any) string {
		t.Helper()
		token := jwt.NewWithClaims(method, claims)
		token.Header["kid"] = kid
		raw, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	claims := func() *Claims {
		options := validOptions()
		now := time.Now()
		return &Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    DefaultIssuer,
				Subject:   options.Subject,
				Audience:  jwt.ClaimStrings{string(options.Audience)},
				ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
				IssuedAt:  jwt.NewNumericDate(now),
				ID:        options.JTI,
			},
			Type: options.Type,
			Role: options.Role,
		}
	}

	cases := []struct {
		name     string
		raw      string
		audience Audience
		want     string
	}{
		{"expired", signWith(fixture.private, claimsWithExpiry(claims(), time.Now().Add(-time.Minute)), fixture.kid), AudienceGatewayAdmin, "expired"},
		{"wrong audience", signWith(fixture.private, claimsWithAudience(claims(), "other"), fixture.kid), AudienceGatewayAdmin, "audience"},
		{"alg none", sign(claims(), fixture.kid, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType), AudienceGatewayAdmin, "signing method"},
		{"HS confusion", sign(claims(), fixture.kid, jwt.SigningMethodHS256, []byte(fixture.key)), AudienceGatewayAdmin, "signing method"},
		{"unknown kid", sign(claims(), "unknown", jwt.SigningMethodEdDSA, testPrivateKey(t)), AudienceGatewayAdmin, "kid"},
		{"missing exp", signWith(fixture.private, claimsWithoutExp(claims()), fixture.kid), AudienceGatewayAdmin, "exp"},
		{"missing jti", signWith(fixture.private, claimsWithoutJTI(claims()), fixture.kid), AudienceGatewayAdmin, "jti"},
		{"revoked jti", signWith(fixture.private, claimsWithJTI(claims(), "revoked-1"), fixture.kid), AudienceGatewayAdmin, "revoked"},
		{"wrong issuer", signWith(fixture.private, claimsWithIssuer(claims(), "other"), fixture.kid), AudienceGatewayAdmin, "issuer"},
		{"bad role", signWith(fixture.private, claimsWithRole(claims(), "admin"), fixture.kid), AudienceGatewayAdmin, "role"},
		{"bad type", signWith(fixture.private, claimsWithType(claims(), "robot"), fixture.kid), AudienceGatewayAdmin, "type"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := fixture.verifier.Verify(test.raw, test.audience); err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func testPrivateKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return private
}

func signWith(private ed25519.PrivateKey, claims *Claims, kid string) string {
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = kid
	raw, err := token.SignedString(private)
	if err != nil {
		panic(err)
	}
	return raw
}

func claimsWithExpiry(c *Claims, at time.Time) *Claims {
	c.ExpiresAt = jwt.NewNumericDate(at)
	return c
}
func claimsWithoutExp(c *Claims) *Claims {
	c.ExpiresAt = nil
	return c
}
func claimsWithAudience(c *Claims, audience string) *Claims {
	c.Audience = jwt.ClaimStrings{audience}
	return c
}
func claimsWithJTI(c *Claims, jti string) *Claims {
	c.ID = jti
	return c
}
func claimsWithoutJTI(c *Claims) *Claims {
	c.ID = ""
	return c
}
func claimsWithIssuer(c *Claims, issuer string) *Claims {
	c.Issuer = issuer
	return c
}
func claimsWithRole(c *Claims, role Role) *Claims {
	c.Role = role
	return c
}
func claimsWithType(c *Claims, typ PrincipalType) *Claims {
	c.Type = typ
	return c
}

func TestLooksLikeJWT(t *testing.T) {
	t.Parallel()
	fixture := newJWTFixture(t)
	raw, err := fixture.signer.Issue(validOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLikeJWT(raw) {
		t.Error("signed JWT was not recognized")
	}
	if LooksLikeJWT("opaque.static.token") {
		t.Error("two-dot unparseable string was treated as a JWT")
	}
	if LooksLikeJWT("static-admin-token") {
		t.Error("opaque static token was treated as a JWT")
	}
}

func TestParsePublicKeys(t *testing.T) {
	t.Parallel()
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded := PublicKeyBase64(public)
	keys, err := ParsePublicKeys("k1=" + encoded)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %v, err = %v", keys, err)
	}
	if _, err := ParsePublicKeys("k1=not-base64"); err == nil {
		t.Error("expected invalid key to fail")
	}
	if _, err := ParsePublicKeys("k1=" + encoded + ",k1=" + encoded); err == nil {
		t.Error("expected duplicate kid to fail")
	}
}

func TestAuthenticatorModes(t *testing.T) {
	t.Parallel()
	fixture := newJWTFixture(t)
	raw, err := fixture.signer.Issue(validOptions())
	if err != nil {
		t.Fatal(err)
	}

	config := Config{Mode: ModeLegacy, Issuer: DefaultIssuer}
	legacy, err := NewAuthenticator(config, AudienceGatewayAdmin, "static-secret")
	if err != nil {
		t.Fatal(err)
	}
	if principal, err := legacy.Authenticate("static-secret"); err != nil || principal.Role != RoleFull || !principal.Static {
		t.Fatalf("legacy static = %+v, %v", principal, err)
	}
	if _, err := legacy.Authenticate(raw); err == nil {
		t.Error("legacy accepted a JWT")
	}

	config.Mode = ModeHybrid
	config.PublicKeys = map[string]ed25519.PublicKey{fixture.kid: fixture.key}
	hybrid, err := NewAuthenticator(config, AudienceGatewayAdmin, "static-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hybrid.Authenticate("static-secret"); err != nil {
		t.Fatalf("hybrid rejected static token: %v", err)
	}
	if _, err := hybrid.Authenticate(raw); err != nil {
		t.Fatalf("hybrid rejected JWT: %v", err)
	}

	config.Mode = ModeJWT
	strict, err := NewAuthenticator(config, AudienceGatewayAdmin, "static-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Authenticate("static-secret"); err == nil {
		t.Error("jwt mode accepted static token")
	}
	if _, err := strict.Authenticate(raw); err != nil {
		t.Fatalf("jwt mode rejected JWT: %v", err)
	}
}

func TestMiddlewareStatusCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := newJWTFixture(t)
	raw, err := fixture.signer.Issue(validOptions())
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Mode: ModeJWT, Issuer: DefaultIssuer, PublicKeys: map[string]ed25519.PublicKey{fixture.kid: fixture.key}}
	authenticator, err := NewAuthenticator(config, AudienceGatewayAdmin, "")
	if err != nil {
		t.Fatal(err)
	}
	abort := func(c *gin.Context, status int, code, message string) {
		c.AbortWithStatusJSON(status, gin.H{"code": code, "message": message})
	}
	router := gin.New()
	group := router.Group("/", Middleware(authenticator, abort))
	group.GET("/read", Require(PermIncidentRead, abort), func(c *gin.Context) { c.Status(http.StatusOK) })
	group.PUT("/write", Require(PermGatewayRateLimitWrite, abort), func(c *gin.Context) { c.Status(http.StatusOK) })

	cases := []struct {
		name   string
		method string
		path   string
		token  string
		want   int
	}{
		{"missing token", http.MethodGet, "/read", "", http.StatusUnauthorized},
		{"invalid token", http.MethodGet, "/read", "garbage", http.StatusUnauthorized},
		{"allowed", http.MethodGet, "/read", raw, http.StatusOK},
		{"forbidden", http.MethodPut, "/write", raw, http.StatusForbidden},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}
