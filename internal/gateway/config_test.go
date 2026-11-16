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
