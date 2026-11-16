package gateway

import (
	"testing"
	"time"
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
