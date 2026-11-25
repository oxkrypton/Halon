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

func TestLoadConfigStoredToolOutput(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("AGENT_MAX_STORED_TOOL_OUTPUT", "131072")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxStoredToolOutput != 131072 {
		t.Fatalf("MaxStoredToolOutput = %d, want 131072", cfg.MaxStoredToolOutput)
	}
}

func TestLoadConfigStoredToolOutputDefaults(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxStoredToolOutput != 256*1024 {
		t.Fatalf("default MaxStoredToolOutput = %d, want %d", cfg.MaxStoredToolOutput, 256*1024)
	}
}

func TestLoadConfigRejectsNonPositiveStoredToolOutput(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("AGENT_MAX_STORED_TOOL_OUTPUT", "0")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() succeeded with AGENT_MAX_STORED_TOOL_OUTPUT=0")
	}
}

func TestLoadConfigRecoveryMaxAge(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("AGENT_RECOVERY_MAX_AGE", "2h")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryMaxAge != 2*time.Hour {
		t.Fatalf("RecoveryMaxAge = %v, want 2h", cfg.RecoveryMaxAge)
	}
}

func TestLoadConfigRecoveryMaxAgeDefaults(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryMaxAge != 30*time.Minute {
		t.Fatalf("default RecoveryMaxAge = %v, want 30m", cfg.RecoveryMaxAge)
	}
}

func TestLoadConfigRejectsNonPositiveRecoveryMaxAge(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("AGENT_RECOVERY_MAX_AGE", "0s")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() succeeded with AGENT_RECOVERY_MAX_AGE=0s")
	}
}

func TestLoadConfigSuppressionDefaults(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CoalesceCooldown != 5*time.Minute || cfg.RetryBackoff != time.Minute || cfg.SettleDelay != 15*time.Second {
		t.Fatalf("unexpected suppression defaults: cooldown=%v backoff=%v settle=%v",
			cfg.CoalesceCooldown, cfg.RetryBackoff, cfg.SettleDelay)
	}
}

func TestLoadConfigSuppressionOverridesAndDisable(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("INCIDENT_COALESCE_COOLDOWN", "30s")
	t.Setenv("INCIDENT_SETTLE_DELAY", "0s")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// RetryBackoff default caps automatically to the shorter cooldown.
	if cfg.CoalesceCooldown != 30*time.Second || cfg.RetryBackoff != 30*time.Second || cfg.SettleDelay != 0 {
		t.Fatalf("unexpected overrides: cooldown=%v backoff=%v settle=%v",
			cfg.CoalesceCooldown, cfg.RetryBackoff, cfg.SettleDelay)
	}

	t.Setenv("INCIDENT_COALESCE_COOLDOWN", "0s")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CoalesceCooldown != 0 || cfg.RetryBackoff != 0 {
		t.Fatalf("cooldown=0s should disable suppression: cooldown=%v backoff=%v", cfg.CoalesceCooldown, cfg.RetryBackoff)
	}
}

func TestLoadConfigRejectsInvalidSuppressionDurations(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("INCIDENT_COALESCE_COOLDOWN", "2m")
	t.Setenv("INCIDENT_RETRY_BACKOFF", "3m")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() succeeded with INCIDENT_RETRY_BACKOFF > INCIDENT_COALESCE_COOLDOWN")
	}

	t.Setenv("INCIDENT_RETRY_BACKOFF", "1m")
	t.Setenv("INCIDENT_SETTLE_DELAY", "-1s")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() succeeded with negative INCIDENT_SETTLE_DELAY")
	}
}
