package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/redis/go-redis/v9"
)

const (
	// retryAttemptsHardCap bounds retry amplification regardless of configuration.
	retryAttemptsHardCap = 3
	minAdminTokenLength  = 12
)

type Config struct {
	HTTPAddr             string
	AdminAddr            string
	Routes               string
	DatabaseDSN          string
	AdminToken           string
	Auth                 authz.Config
	AgentWebhookURL      string
	AgentWebhookToken    string
	GinMode              string
	LogLevel             string
	MetricsRequireAuth   bool
	RateLimitRPS         float64
	RateLimitBurst       int
	LogQueueSize         int
	LogBatchSize         int
	TimeoutQueueSize     int
	LogFlushInterval     time.Duration
	ProxyTimeout         time.Duration
	SlowRequestThreshold time.Duration
	BodyCaptureLimit     int64
	ShutdownTimeout      time.Duration

	// Upstream transport (P0).
	UpstreamDialTimeout           time.Duration
	UpstreamTLSHandshakeTimeout   time.Duration
	UpstreamResponseHeaderTimeout time.Duration
	UpstreamMaxConnsPerHost       int

	// Resilience policies (P1).
	MaxRequestBodyBytes      int64
	RetryMaxAttempts         int
	RetryBodyBufferBytes     int64
	BreakerEnabled           bool
	BreakerFailureThreshold  uint
	BreakerOpenDuration      time.Duration
	BreakerHalfOpenSuccesses uint
	BreakerCount5xx          bool
	ConcurrencyPerRoute      uint

	// Tracing (P2).
	OTELEnabled            bool
	OTELExporterEndpoint   string
	OTELTracesSamplerRatio float64

	// Shared state (optional). RedisURL empty keeps the single-replica
	// in-memory mode; non-empty enables the shared multi-replica mode.
	RedisURL          string
	BlacklistCacheTTL time.Duration
	RedisDialTimeout  time.Duration
	RedisOpTimeout    time.Duration
}

func LoadConfig() (Config, error) {
	cfg := Config{
		HTTPAddr:             envString("HTTP_ADDR", ":8080"),
		AdminAddr:            envString("ADMIN_ADDR", ":9090"),
		Routes:               os.Getenv("ROUTES"),
		DatabaseDSN:          os.Getenv("DATABASE_DSN"),
		AdminToken:           os.Getenv("ADMIN_TOKEN"),
		AgentWebhookURL:      os.Getenv("AGENT_WEBHOOK_URL"),
		AgentWebhookToken:    os.Getenv("AGENT_WEBHOOK_TOKEN"),
		GinMode:              envString("GIN_MODE", "release"),
		LogLevel:             envString("LOG_LEVEL", "info"),
		RateLimitRPS:         20,
		RateLimitBurst:       40,
		LogQueueSize:         1024,
		LogBatchSize:         500,
		TimeoutQueueSize:     64,
		LogFlushInterval:     time.Second,
		ProxyTimeout:         3 * time.Second,
		SlowRequestThreshold: time.Second,
		BodyCaptureLimit:     64 * 1024,
		ShutdownTimeout:      10 * time.Second,

		UpstreamDialTimeout:         2 * time.Second,
		UpstreamTLSHandshakeTimeout: 3 * time.Second,
		UpstreamMaxConnsPerHost:     64,

		MaxRequestBodyBytes:      10 << 20,
		RetryMaxAttempts:         1,
		RetryBodyBufferBytes:     1 << 20,
		BreakerEnabled:           true,
		BreakerFailureThreshold:  5,
		BreakerOpenDuration:      30 * time.Second,
		BreakerHalfOpenSuccesses: 1,
		BreakerCount5xx:          true,
		ConcurrencyPerRoute:      256,

		// otlptracegrpc expects host:port without a scheme.
		OTELExporterEndpoint:   "otel-collector.observability.svc.cluster.local:4317",
		OTELTracesSamplerRatio: 1.0,

		RedisURL:          os.Getenv("REDIS_URL"),
		BlacklistCacheTTL: 10 * time.Second,
		RedisDialTimeout:  time.Second,
		RedisOpTimeout:    500 * time.Millisecond,
	}

	var err error
	if cfg.MetricsRequireAuth, err = envBool("METRICS_REQUIRE_AUTH", cfg.MetricsRequireAuth); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitRPS, err = envFloat("RATE_LIMIT_RPS", cfg.RateLimitRPS); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitBurst, err = envInt("RATE_LIMIT_BURST", cfg.RateLimitBurst); err != nil {
		return Config{}, err
	}
	if cfg.LogQueueSize, err = envInt("LOG_QUEUE_SIZE", cfg.LogQueueSize); err != nil {
		return Config{}, err
	}
	if cfg.LogBatchSize, err = envInt("LOG_BATCH_SIZE", cfg.LogBatchSize); err != nil {
		return Config{}, err
	}
	if cfg.TimeoutQueueSize, err = envInt("TIMEOUT_QUEUE_SIZE", cfg.TimeoutQueueSize); err != nil {
		return Config{}, err
	}
	if cfg.LogFlushInterval, err = envDuration("LOG_FLUSH_INTERVAL", cfg.LogFlushInterval); err != nil {
		return Config{}, err
	}
	if cfg.ProxyTimeout, err = envDuration("UPSTREAM_TIMEOUT", cfg.ProxyTimeout); err != nil {
		return Config{}, err
	}
	if cfg.SlowRequestThreshold, err = envDuration("SLOW_REQUEST_THRESHOLD", cfg.SlowRequestThreshold); err != nil {
		return Config{}, err
	}
	if cfg.BodyCaptureLimit, err = envInt64("BODY_CAPTURE_BYTES", cfg.BodyCaptureLimit); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}

	// Upstream transport. The response header timeout defaults to the overall
	// proxy timeout and must never exceed it, so it is parsed afterwards.
	if cfg.UpstreamDialTimeout, err = envDuration("UPSTREAM_DIAL_TIMEOUT", cfg.UpstreamDialTimeout); err != nil {
		return Config{}, err
	}
	if cfg.UpstreamTLSHandshakeTimeout, err = envDuration("UPSTREAM_TLS_HANDSHAKE_TIMEOUT", cfg.UpstreamTLSHandshakeTimeout); err != nil {
		return Config{}, err
	}
	if cfg.UpstreamResponseHeaderTimeout, err = envDuration("UPSTREAM_RESPONSE_HEADER_TIMEOUT", cfg.ProxyTimeout); err != nil {
		return Config{}, err
	}
	if cfg.UpstreamMaxConnsPerHost, err = envInt("UPSTREAM_MAX_CONNS_PER_HOST", cfg.UpstreamMaxConnsPerHost); err != nil {
		return Config{}, err
	}

	// Resilience policies.
	if cfg.MaxRequestBodyBytes, err = envInt64("MAX_REQUEST_BODY_BYTES", cfg.MaxRequestBodyBytes); err != nil {
		return Config{}, err
	}
	if cfg.RetryMaxAttempts, err = envInt("RETRY_MAX_ATTEMPTS", cfg.RetryMaxAttempts); err != nil {
		return Config{}, err
	}
	if cfg.RetryBodyBufferBytes, err = envInt64("RETRY_BODY_BUFFER_BYTES", cfg.RetryBodyBufferBytes); err != nil {
		return Config{}, err
	}
	if cfg.BreakerEnabled, err = envBool("BREAKER_ENABLED", cfg.BreakerEnabled); err != nil {
		return Config{}, err
	}
	var breakerFailureThreshold int
	if breakerFailureThreshold, err = envInt("BREAKER_FAILURE_THRESHOLD", int(cfg.BreakerFailureThreshold)); err != nil {
		return Config{}, err
	}
	cfg.BreakerFailureThreshold = uint(breakerFailureThreshold)
	if cfg.BreakerOpenDuration, err = envDuration("BREAKER_OPEN_DURATION", cfg.BreakerOpenDuration); err != nil {
		return Config{}, err
	}
	var breakerHalfOpenSuccesses int
	if breakerHalfOpenSuccesses, err = envInt("BREAKER_HALF_OPEN_MAX", int(cfg.BreakerHalfOpenSuccesses)); err != nil {
		return Config{}, err
	}
	cfg.BreakerHalfOpenSuccesses = uint(breakerHalfOpenSuccesses)
	if cfg.BreakerCount5xx, err = envBool("BREAKER_COUNT_5XX", cfg.BreakerCount5xx); err != nil {
		return Config{}, err
	}
	var concurrencyPerRoute int
	if concurrencyPerRoute, err = envInt("CONCURRENCY_PER_ROUTE", int(cfg.ConcurrencyPerRoute)); err != nil {
		return Config{}, err
	}
	cfg.ConcurrencyPerRoute = uint(concurrencyPerRoute)

	// Tracing.
	if cfg.OTELEnabled, err = envBool("OTEL_ENABLED", cfg.OTELEnabled); err != nil {
		return Config{}, err
	}
	cfg.OTELExporterEndpoint = envString("OTEL_EXPORTER_OTLP_ENDPOINT", cfg.OTELExporterEndpoint)
	if cfg.OTELTracesSamplerRatio, err = envFloat("OTEL_TRACES_SAMPLER_RATIO", cfg.OTELTracesSamplerRatio); err != nil {
		return Config{}, err
	}

	// Shared state (optional).
	if cfg.BlacklistCacheTTL, err = envDuration("BLACKLIST_CACHE_TTL", cfg.BlacklistCacheTTL); err != nil {
		return Config{}, err
	}
	if cfg.RedisDialTimeout, err = envDuration("REDIS_DIAL_TIMEOUT", cfg.RedisDialTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RedisOpTimeout, err = envDuration("REDIS_OP_TIMEOUT", cfg.RedisOpTimeout); err != nil {
		return Config{}, err
	}

	authConfig, err := authz.LoadConfig()
	if err != nil {
		return Config{}, err
	}
	cfg.Auth = authConfig

	var validationErrors []error
	if _, err := ParseRoutes(cfg.Routes); err != nil {
		validationErrors = append(validationErrors, fmt.Errorf("ROUTES: %w", err))
	}
	if cfg.DatabaseDSN == "" {
		validationErrors = append(validationErrors, errors.New("DATABASE_DSN is required"))
	}
	if cfg.Auth.Mode != authz.ModeJWT && len(cfg.AdminToken) < minAdminTokenLength {
		validationErrors = append(validationErrors, errors.New("ADMIN_TOKEN must contain at least 12 characters in legacy and hybrid modes"))
	}
	if cfg.AgentWebhookURL != "" && len(cfg.AgentWebhookToken) < minAdminTokenLength {
		validationErrors = append(validationErrors, errors.New("AGENT_WEBHOOK_TOKEN must contain at least 12 characters when AGENT_WEBHOOK_URL is set"))
	}
	if cfg.GinMode != "release" && cfg.GinMode != "debug" && cfg.GinMode != "test" {
		validationErrors = append(validationErrors, errors.New("GIN_MODE must be release, debug, or test"))
	}
	if _, ok := slogLevels[cfg.LogLevel]; !ok {
		validationErrors = append(validationErrors, errors.New("LOG_LEVEL must be debug, info, warn, or error"))
	}
	if math.IsNaN(cfg.RateLimitRPS) || math.IsInf(cfg.RateLimitRPS, 0) || cfg.RateLimitRPS <= 0 || cfg.RateLimitBurst <= 0 {
		validationErrors = append(validationErrors, errors.New("rate limit values must be positive"))
	}
	if cfg.LogQueueSize <= 0 || cfg.LogBatchSize <= 0 || cfg.TimeoutQueueSize <= 0 || cfg.LogFlushInterval <= 0 {
		validationErrors = append(validationErrors, errors.New("log queues, batch size, and flush interval must be positive"))
	}
	if cfg.ProxyTimeout <= 0 || cfg.SlowRequestThreshold <= 0 || cfg.BodyCaptureLimit <= 0 || cfg.ShutdownTimeout <= 0 {
		validationErrors = append(validationErrors, errors.New("timeouts and body capture limit must be positive"))
	}
	if cfg.UpstreamDialTimeout <= 0 || cfg.UpstreamTLSHandshakeTimeout <= 0 {
		validationErrors = append(validationErrors, errors.New("upstream dial and TLS handshake timeouts must be positive"))
	}
	if cfg.UpstreamResponseHeaderTimeout <= 0 || cfg.UpstreamResponseHeaderTimeout > cfg.ProxyTimeout {
		validationErrors = append(validationErrors, errors.New("UPSTREAM_RESPONSE_HEADER_TIMEOUT must be positive and not exceed UPSTREAM_TIMEOUT"))
	}
	if cfg.UpstreamMaxConnsPerHost <= 0 {
		validationErrors = append(validationErrors, errors.New("UPSTREAM_MAX_CONNS_PER_HOST must be positive"))
	}
	if cfg.MaxRequestBodyBytes < 0 {
		validationErrors = append(validationErrors, errors.New("MAX_REQUEST_BODY_BYTES must be zero (unlimited) or positive"))
	}
	if cfg.RetryMaxAttempts < 1 || cfg.RetryMaxAttempts > retryAttemptsHardCap {
		validationErrors = append(validationErrors, fmt.Errorf("RETRY_MAX_ATTEMPTS must be 1..%d", retryAttemptsHardCap))
	}
	if cfg.RetryBodyBufferBytes <= 0 {
		validationErrors = append(validationErrors, errors.New("RETRY_BODY_BUFFER_BYTES must be positive"))
	}
	if cfg.BreakerFailureThreshold == 0 || cfg.BreakerOpenDuration <= 0 || cfg.BreakerHalfOpenSuccesses == 0 {
		validationErrors = append(validationErrors, errors.New("breaker threshold, open duration, and half-open successes must be positive"))
	}
	if cfg.OTELEnabled && cfg.OTELExporterEndpoint == "" {
		validationErrors = append(validationErrors, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT is required when OTEL_ENABLED is true"))
	}
	if math.IsNaN(cfg.OTELTracesSamplerRatio) || cfg.OTELTracesSamplerRatio < 0 || cfg.OTELTracesSamplerRatio > 1 {
		validationErrors = append(validationErrors, errors.New("OTEL_TRACES_SAMPLER_RATIO must be between 0 and 1"))
	}
	if cfg.RedisURL != "" {
		if _, err := redis.ParseURL(cfg.RedisURL); err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("REDIS_URL: %w", err))
		}
	}
	if cfg.BlacklistCacheTTL <= 0 || cfg.RedisDialTimeout <= 0 || cfg.RedisOpTimeout <= 0 {
		validationErrors = append(validationErrors, errors.New("blacklist cache TTL and redis timeouts must be positive"))
	}
	return cfg, errors.Join(validationErrors...)
}

var slogLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// SlogLevel returns the configured log level; LoadConfig guarantees it is valid.
func (c Config) SlogLevel() slog.Level {
	return slogLevels[c.LogLevel]
}

func envString(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) (bool, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
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

func envInt64(key string, fallback int64) (int64, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
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
