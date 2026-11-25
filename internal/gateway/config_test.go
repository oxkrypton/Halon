package gateway

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/oxkrypton/Halon/internal/authz"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("ADMIN_TOKEN", "test-admin-token")
	t.Setenv("RATE_LIMIT_RPS", "")
	t.Setenv("RATE_LIMIT_BURST", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.AdminAddr != ":9090" || cfg.LogBatchSize != 500 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.LogFlushInterval != time.Second || cfg.BodyCaptureLimit != 64*1024 {
		t.Fatalf("unexpected duration defaults: %+v", cfg)
	}
}

func TestLoadConfigRequiresSecrets(t *testing.T) {
	t.Setenv("DATABASE_DSN", "")
	t.Setenv("ADMIN_TOKEN", "short")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() succeeded with missing DSN and short token")
	}
}

func TestLoadConfigResilienceDefaults(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("ADMIN_TOKEN", "test-admin-token")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "info" || cfg.SlogLevel() != 0 {
		t.Fatalf("unexpected log level defaults: %+v", cfg.LogLevel)
	}
	if cfg.UpstreamDialTimeout != 2*time.Second || cfg.UpstreamTLSHandshakeTimeout != 3*time.Second {
		t.Fatalf("unexpected transport timeout defaults: %+v", cfg)
	}
	if cfg.UpstreamResponseHeaderTimeout != cfg.ProxyTimeout {
		t.Fatalf("response header timeout %v must default to the proxy timeout %v", cfg.UpstreamResponseHeaderTimeout, cfg.ProxyTimeout)
	}
	if cfg.UpstreamMaxConnsPerHost != 64 || cfg.MaxRequestBodyBytes != 10<<20 {
		t.Fatalf("unexpected transport limit defaults: %+v", cfg)
	}
	if cfg.RetryMaxAttempts != 1 || cfg.RetryBodyBufferBytes != 1<<20 {
		t.Fatalf("unexpected retry defaults: %+v", cfg)
	}
	if !cfg.BreakerEnabled || cfg.BreakerFailureThreshold != 5 || cfg.BreakerOpenDuration != 30*time.Second ||
		cfg.BreakerHalfOpenSuccesses != 1 || !cfg.BreakerCount5xx {
		t.Fatalf("unexpected breaker defaults: %+v", cfg)
	}
	if cfg.ConcurrencyPerRoute != 256 {
		t.Fatalf("unexpected concurrency default: %+v", cfg)
	}
	if cfg.OTELEnabled || cfg.OTELExporterEndpoint == "" || cfg.OTELTracesSamplerRatio != 1.0 {
		t.Fatalf("unexpected OTel defaults: %+v", cfg)
	}
}

func TestLoadConfigRejectsInvalidResilience(t *testing.T) {
	cases := map[string]map[string]string{
		"log level":          {"LOG_LEVEL": "verbose"},
		"retry attempts":     {"RETRY_MAX_ATTEMPTS": "4"},
		"breaker threshold":  {"BREAKER_FAILURE_THRESHOLD": "0"},
		"header timeout":     {"UPSTREAM_RESPONSE_HEADER_TIMEOUT": "10s"},
		"sampler ratio":      {"OTEL_TRACES_SAMPLER_RATIO": "1.5"},
		"negative body cap":  {"MAX_REQUEST_BODY_BYTES": "-1"},
		"negative dial":      {"UPSTREAM_DIAL_TIMEOUT": "-1s"},
		"boolean parse":      {"BREAKER_ENABLED": "not-a-bool"},
		"max conns per host": {"UPSTREAM_MAX_CONNS_PER_HOST": "0"},
	}
	for name, envs := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
			t.Setenv("ADMIN_TOKEN", "test-admin-token")
			for key, value := range envs {
				t.Setenv(key, value)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("LoadConfig() succeeded with %v", envs)
			}
		})
	}
}

func TestLoadConfigResilienceOverrides(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("ADMIN_TOKEN", "test-admin-token")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("RETRY_MAX_ATTEMPTS", "3")
	t.Setenv("BREAKER_ENABLED", "false")
	t.Setenv("CONCURRENCY_PER_ROUTE", "0")
	t.Setenv("UPSTREAM_RESPONSE_HEADER_TIMEOUT", "1500ms")
	t.Setenv("OTEL_ENABLED", "true")
	t.Setenv("OTEL_TRACES_SAMPLER_RATIO", "0.25")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" || cfg.RetryMaxAttempts != 3 || cfg.BreakerEnabled {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.ConcurrencyPerRoute != 0 || cfg.UpstreamResponseHeaderTimeout != 1500*time.Millisecond {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if !cfg.OTELEnabled || cfg.OTELTracesSamplerRatio != 0.25 {
		t.Fatalf("OTel overrides not applied: %+v", cfg)
	}
}

func TestLoadConfigSharedState(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("ADMIN_TOKEN", "test-admin-token")
	t.Setenv("REDIS_URL", "redis://redis:6379/2")
	t.Setenv("BLACKLIST_CACHE_TTL", "5s")
	t.Setenv("REDIS_OP_TIMEOUT", "250ms")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisURL != "redis://redis:6379/2" || cfg.BlacklistCacheTTL != 5*time.Second || cfg.RedisOpTimeout != 250*time.Millisecond {
		t.Fatalf("shared state overrides not applied: %+v", cfg)
	}
}

func TestLoadConfigSharedStateDefaults(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("ADMIN_TOKEN", "test-admin-token")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisURL != "" || cfg.BlacklistCacheTTL != 10*time.Second || cfg.RedisDialTimeout != time.Second || cfg.RedisOpTimeout != 500*time.Millisecond {
		t.Fatalf("unexpected shared state defaults: %+v", cfg)
	}
}

func TestLoadConfigRejectsInvalidSharedState(t *testing.T) {
	cases := map[string]map[string]string{
		"unparseable redis url": {"REDIS_URL": "not a url"},
		"zero cache ttl":        {"BLACKLIST_CACHE_TTL": "0s"},
		"negative op timeout":   {"REDIS_OP_TIMEOUT": "-1s"},
	}
	for name, envs := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
			t.Setenv("ADMIN_TOKEN", "test-admin-token")
			for key, value := range envs {
				t.Setenv(key, value)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("LoadConfig() succeeded with %v", envs)
			}
		})
	}
}

func TestIPRulesValidation(t *testing.T) {
	rules, err := NewIPRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rules.Add("not-an-ip"); err == nil {
		t.Fatal("Add() accepted invalid IP")
	}
	created, err := rules.Add("2001:0db8::1")
	if err != nil || !created {
		t.Fatalf("Add(IPv6) = %v, %v", created, err)
	}
	if !rules.Contains("2001:db8::1") {
		t.Fatal("canonical IPv6 address was not matched")
	}
	created, _ = rules.Add("2001:db8::1")
	if created {
		t.Fatal("duplicate rule reported as created")
	}
}

func loginTestPrivateKey(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return authz.PrivateKeyBase64(private), public
}

func setLoginConfig(t *testing.T, privateKey string) {
	t.Helper()
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("AUTH_MODE", "jwt")
	t.Setenv("AUTH_LOGIN_ENABLED", "true")
	t.Setenv("AUTH_JWT_PRIVATE_KEY", privateKey)
}

func TestLoadConfigLoginDefaults(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("ADMIN_TOKEN", "test-admin-token")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoginEnabled || cfg.JWTPrivateKey != "" {
		t.Fatalf("login must be disabled by default: %+v", cfg)
	}
	if cfg.AccessTokenTTL != 15*time.Minute || cfg.RefreshTokenTTL != 168*time.Hour ||
		cfg.RefreshMaxLifetime != 720*time.Hour || cfg.PasswordMinLength != 12 {
		t.Fatalf("unexpected login defaults: %+v", cfg)
	}
	if cfg.LoginRateLimitRPS != 0.2 || cfg.LoginRateLimitBurst != 5 ||
		cfg.LoginMaxFailures != 10 || cfg.LoginLockDuration != 5*time.Minute ||
		cfg.AuthEventRetention != 2160*time.Hour {
		t.Fatalf("unexpected login limit defaults: %+v", cfg)
	}
}

func TestLoadConfigLoginMergesSigningKey(t *testing.T) {
	privateKey, public := loginTestPrivateKey(t)
	setLoginConfig(t, privateKey)
	t.Setenv("AUTH_ACCESS_TOKEN_TTL", "30m")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LoginEnabled || cfg.JWTKID != authz.KeyID(public) || cfg.AccessTokenTTL != 30*time.Minute {
		t.Fatalf("unexpected login config: enabled=%v kid=%q ttl=%v", cfg.LoginEnabled, cfg.JWTKID, cfg.AccessTokenTTL)
	}
	key, found := cfg.Auth.PublicKeys[cfg.JWTKID]
	if !found || !key.Equal(public) {
		t.Fatal("derived signing public key was not merged into the verifier set")
	}
	verifier, err := cfg.Auth.Verifier()
	if err != nil || verifier == nil {
		t.Fatalf("verifier: %v", err)
	}
	signer, err := authz.NewSigner(mustParsePrivateKey(t, privateKey), cfg.JWTKID, cfg.Auth.Issuer)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Issue(authz.IssueOptions{
		Subject: "alice", Type: authz.TypeHuman, Role: authz.RoleFull,
		Audiences: []authz.Audience{authz.AudienceGatewayAdmin}, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(token, authz.AudienceGatewayAdmin); err != nil {
		t.Fatalf("gateway cannot verify its own login token: %v", err)
	}
}

func mustParsePrivateKey(t *testing.T, encoded string) ed25519.PrivateKey {
	t.Helper()
	key, err := authz.ParsePrivateKeyBase64(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestLoadConfigRejectsInvalidLoginConfig(t *testing.T) {
	privateKey, public := loginTestPrivateKey(t)
	otherKey, _ := loginTestPrivateKey(t)
	cases := map[string]map[string]string{
		"legacy mode":           {"AUTH_MODE": "legacy", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": privateKey},
		"missing private key":   {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": ""},
		"malformed private key": {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": "not-base64!"},
		"short access ttl":      {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": privateKey, "AUTH_ACCESS_TOKEN_TTL": "30s"},
		"long access ttl":       {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": privateKey, "AUTH_ACCESS_TOKEN_TTL": "2h"},
		"weak password policy":  {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": privateKey, "AUTH_PASSWORD_MIN_LENGTH": "4"},
		"zero login rps":        {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": privateKey, "AUTH_LOGIN_RATE_LIMIT_RPS": "0"},
		"zero max failures":     {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": privateKey, "AUTH_LOGIN_MAX_FAILURES": "0"},
		"bootstrap user only":   {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": privateKey, "AUTH_BOOTSTRAP_ADMIN_USER": "admin"},
		"bootstrap weak pass":   {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": privateKey, "AUTH_BOOTSTRAP_ADMIN_USER": "admin", "AUTH_BOOTSTRAP_ADMIN_PASSWORD": "short"},
		"bootstrap bad user":    {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": privateKey, "AUTH_BOOTSTRAP_ADMIN_USER": "ADMIN", "AUTH_BOOTSTRAP_ADMIN_PASSWORD": "long-enough-password"},
		"kid conflict":          {"AUTH_MODE": "jwt", "AUTH_LOGIN_ENABLED": "true", "AUTH_JWT_PRIVATE_KEY": otherKey, "AUTH_JWT_KID": authz.KeyID(public), "AUTH_JWT_PUBLIC_KEYS": authz.KeyID(public) + "=" + authz.PublicKeyBase64(public)},
	}
	for name, envs := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
			for key, value := range envs {
				t.Setenv(key, value)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("LoadConfig() succeeded with %v", envs)
			}
		})
	}
}

func TestLoadConfigLoginExplicitKIDAndBootstrap(t *testing.T) {
	privateKey, _ := loginTestPrivateKey(t)
	setLoginConfig(t, privateKey)
	t.Setenv("AUTH_JWT_KID", "login-key-1")
	t.Setenv("AUTH_BOOTSTRAP_ADMIN_USER", "admin")
	t.Setenv("AUTH_BOOTSTRAP_ADMIN_PASSWORD", "bootstrap-secret-phrase")
	t.Setenv("AUTH_REFRESH_TOKEN_TTL", "24h")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JWTKID != "login-key-1" || cfg.BootstrapAdminUser != "admin" ||
		cfg.BootstrapAdminPassword != "bootstrap-secret-phrase" || cfg.RefreshTokenTTL != 24*time.Hour {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if _, found := cfg.Auth.PublicKeys["login-key-1"]; !found {
		t.Fatal("explicit kid was not added to the verifier set")
	}
}
