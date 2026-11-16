package agent

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config holds the agent service configuration loaded from the environment.
type Config struct {
	HTTPAddr          string
	DatabaseDSN       string
	IngestToken       string
	OpenAIBaseURL     string
	OpenAIAPIKey      string
	OpenAIModel       string
	PrometheusURL     string
	GatewayAdminURL   string
	GatewayAdminToken string
	OperationLevel    OperationLevel
	GinMode           string
	K8sNamespaces     []string
	K8sMaxReplicas    int
	K8sActionCooldown time.Duration
	MaxSteps          int
	MaxToolOutput     int
	Workers           int
	QueueSize         int
	RunTimeout        time.Duration
	DedupTTL          time.Duration
	OpenAITimeout     time.Duration
	ShutdownTimeout   time.Duration

	// Shared state (optional). RedisURL empty keeps the single-replica
	// in-memory mode; non-empty enables the shared multi-replica mode.
	RedisURL         string
	RedisDialTimeout time.Duration
	RedisOpTimeout   time.Duration
}

func LoadConfig() (Config, error) {
	cfg := Config{
		HTTPAddr:          envString("HTTP_ADDR", ":8081"),
		DatabaseDSN:       os.Getenv("DATABASE_DSN"),
		IngestToken:       os.Getenv("INGEST_TOKEN"),
		OpenAIBaseURL:     envString("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		OpenAIAPIKey:      os.Getenv("OPENAI_API_KEY"),
		OpenAIModel:       os.Getenv("OPENAI_MODEL"),
		PrometheusURL:     os.Getenv("PROMETHEUS_URL"),
		GatewayAdminURL:   os.Getenv("GATEWAY_ADMIN_URL"),
		GatewayAdminToken: os.Getenv("GATEWAY_ADMIN_TOKEN"),
		GinMode:           envString("GIN_MODE", "release"),
		K8sNamespaces:     splitCSV(envString("K8S_NAMESPACES", "default")),
		K8sMaxReplicas:    5,
		K8sActionCooldown: 2 * time.Minute,
		MaxSteps:          12,
		MaxToolOutput:     4096,
		Workers:           2,
		QueueSize:         64,
		RunTimeout:        5 * time.Minute,
		DedupTTL:          10 * time.Minute,
		OpenAITimeout:     60 * time.Second,
		ShutdownTimeout:   30 * time.Second,
		RedisURL:          os.Getenv("REDIS_URL"),
		RedisDialTimeout:  time.Second,
		RedisOpTimeout:    500 * time.Millisecond,
	}

	var err error
	if cfg.K8sMaxReplicas, err = envInt("K8S_MAX_REPLICAS", cfg.K8sMaxReplicas); err != nil {
		return Config{}, err
	}
	if cfg.K8sActionCooldown, err = envDuration("K8S_ACTION_COOLDOWN", cfg.K8sActionCooldown); err != nil {
		return Config{}, err
	}
	if cfg.MaxSteps, err = envInt("AGENT_MAX_STEPS", cfg.MaxSteps); err != nil {
		return Config{}, err
	}
	if cfg.MaxToolOutput, err = envInt("AGENT_MAX_TOOL_OUTPUT", cfg.MaxToolOutput); err != nil {
		return Config{}, err
	}
	if cfg.Workers, err = envInt("AGENT_WORKERS", cfg.Workers); err != nil {
		return Config{}, err
	}
	if cfg.QueueSize, err = envInt("AGENT_QUEUE_SIZE", cfg.QueueSize); err != nil {
		return Config{}, err
	}
	if cfg.RunTimeout, err = envDuration("AGENT_RUN_TIMEOUT", cfg.RunTimeout); err != nil {
		return Config{}, err
	}
	if cfg.DedupTTL, err = envDuration("AGENT_DEDUP_TTL", cfg.DedupTTL); err != nil {
		return Config{}, err
	}
	if cfg.OpenAITimeout, err = envDuration("OPENAI_TIMEOUT", cfg.OpenAITimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RedisDialTimeout, err = envDuration("REDIS_DIAL_TIMEOUT", cfg.RedisDialTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RedisOpTimeout, err = envDuration("REDIS_OP_TIMEOUT", cfg.RedisOpTimeout); err != nil {
		return Config{}, err
	}
	if cfg.OperationLevel, err = ParseOperationLevel(envString("AGENT_OPERATION_LEVEL", string(LevelReadOnly))); err != nil {
		return Config{}, err
	}

	var validationErrors []error
	if cfg.DatabaseDSN == "" {
		validationErrors = append(validationErrors, errors.New("DATABASE_DSN is required"))
	}
	if len(cfg.IngestToken) < 12 {
		validationErrors = append(validationErrors, errors.New("INGEST_TOKEN must contain at least 12 characters"))
	}
	if cfg.OpenAIAPIKey == "" {
		validationErrors = append(validationErrors, errors.New("OPENAI_API_KEY is required"))
	}
	if cfg.OpenAIModel == "" {
		validationErrors = append(validationErrors, errors.New("OPENAI_MODEL is required"))
	}
	if cfg.GinMode != "release" && cfg.GinMode != "debug" && cfg.GinMode != "test" {
		validationErrors = append(validationErrors, errors.New("GIN_MODE must be release, debug, or test"))
	}
	if len(cfg.K8sNamespaces) == 0 {
		validationErrors = append(validationErrors, errors.New("K8S_NAMESPACES must allow at least one namespace"))
	}
	if cfg.K8sMaxReplicas <= 0 || cfg.K8sActionCooldown <= 0 {
		validationErrors = append(validationErrors, errors.New("kubernetes limits must be positive"))
	}
	if cfg.MaxSteps <= 0 || cfg.MaxToolOutput <= 0 || cfg.Workers <= 0 || cfg.QueueSize <= 0 {
		validationErrors = append(validationErrors, errors.New("agent steps, tool output, workers, and queue size must be positive"))
	}
	if cfg.RunTimeout <= 0 || cfg.DedupTTL <= 0 || cfg.OpenAITimeout <= 0 || cfg.ShutdownTimeout <= 0 {
		validationErrors = append(validationErrors, errors.New("timeouts and dedup TTL must be positive"))
	}
	if cfg.RedisURL != "" {
		if _, err := redis.ParseURL(cfg.RedisURL); err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("REDIS_URL: %w", err))
		}
	}
	if cfg.RedisDialTimeout <= 0 || cfg.RedisOpTimeout <= 0 {
		validationErrors = append(validationErrors, errors.New("redis timeouts must be positive"))
	}
	return cfg, errors.Join(validationErrors...)
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func envString(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}
