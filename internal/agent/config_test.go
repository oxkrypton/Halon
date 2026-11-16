package agent

import (
	"testing"
	"time"
)

func TestLoadConfigSharedState(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("REDIS_URL", "redis://redis:6379/2")
	t.Setenv("REDIS_OP_TIMEOUT", "250ms")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisURL != "redis://redis:6379/2" || cfg.RedisOpTimeout != 250*time.Millisecond {
		t.Fatalf("shared state overrides not applied: %+v", cfg)
	}
}

func TestLoadConfigSharedStateDefaults(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisURL != "" || cfg.RedisDialTimeout != time.Second || cfg.RedisOpTimeout != 500*time.Millisecond {
		t.Fatalf("unexpected shared state defaults: %+v", cfg)
	}
}

func TestLoadConfigRejectsInvalidRedisURL(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("REDIS_URL", "not a url")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() succeeded with an unparseable REDIS_URL")
	}
}
