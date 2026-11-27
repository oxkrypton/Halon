package agent

import (
	"testing"
	"time"
)

func setRequiredAgentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_DSN", "postgres://localhost/halon")
	t.Setenv("INGEST_TOKEN", "test-ingest-token")
	t.Setenv("OPENAI_API_KEY", "test-api-key")
	t.Setenv("OPENAI_MODEL", "test-model")
}

func TestLoadConfigRecoveryMaxAge(t *testing.T) {
	setRequiredAgentEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryMaxAge != 30*time.Minute {
		t.Fatalf("default RecoveryMaxAge = %v, want 30m", cfg.RecoveryMaxAge)
	}

	t.Setenv("AGENT_RECOVERY_MAX_AGE", "10m")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryMaxAge != 10*time.Minute {
		t.Fatalf("RecoveryMaxAge = %v, want 10m", cfg.RecoveryMaxAge)
	}

	t.Setenv("AGENT_RECOVERY_MAX_AGE", "0s")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() succeeded with AGENT_RECOVERY_MAX_AGE=0s")
	}
}

func TestLoadConfigStoredToolOutput(t *testing.T) {
	setRequiredAgentEnv(t)
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
	setRequiredAgentEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxStoredToolOutput != 256*1024 {
		t.Fatalf("default MaxStoredToolOutput = %d, want %d", cfg.MaxStoredToolOutput, 256*1024)
	}
}

func TestLoadConfigRejectsNonPositiveStoredToolOutput(t *testing.T) {
	setRequiredAgentEnv(t)
	t.Setenv("AGENT_MAX_STORED_TOOL_OUTPUT", "0")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() succeeded with AGENT_MAX_STORED_TOOL_OUTPUT=0")
	}
}
