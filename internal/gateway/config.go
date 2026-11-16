package gateway

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr             string
	AdminAddr            string
	UserServiceAddr      string
	DatabaseDSN          string
	AdminToken           string
	GinMode              string
	RateLimitRPS         float64
	RateLimitBurst       int
	LogQueueSize         int
	LogBatchSize         int
	LogFlushInterval     time.Duration
	RPCTimeout           time.Duration
	SlowRequestThreshold time.Duration
	BodyCaptureLimit     int64
	ShutdownTimeout      time.Duration
}

func LoadConfig() (Config, error) {
	cfg := Config{
		HTTPAddr:             envString("HTTP_ADDR", ":8080"),
		AdminAddr:            envString("ADMIN_ADDR", ":9090"),
		UserServiceAddr:      envString("USER_SERVICE_ADDR", "localhost:50051"),
		DatabaseDSN:          os.Getenv("DATABASE_DSN"),
		AdminToken:           os.Getenv("ADMIN_TOKEN"),
		GinMode:              envString("GIN_MODE", "release"),
		RateLimitRPS:         20,
		RateLimitBurst:       40,
		LogQueueSize:         1024,
		LogBatchSize:         500,
		LogFlushInterval:     time.Second,
		RPCTimeout:           3 * time.Second,
		SlowRequestThreshold: time.Second,
		BodyCaptureLimit:     64 * 1024,
		ShutdownTimeout:      10 * time.Second,
	}

	var err error
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
	if cfg.LogFlushInterval, err = envDuration("LOG_FLUSH_INTERVAL", cfg.LogFlushInterval); err != nil {
		return Config{}, err
	}
	if cfg.RPCTimeout, err = envDuration("RPC_TIMEOUT", cfg.RPCTimeout); err != nil {
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

	var validationErrors []error
	if cfg.DatabaseDSN == "" {
		validationErrors = append(validationErrors, errors.New("DATABASE_DSN is required"))
	}
	if len(cfg.AdminToken) < 12 {
		validationErrors = append(validationErrors, errors.New("ADMIN_TOKEN must contain at least 12 characters"))
	}
	if cfg.GinMode != "release" && cfg.GinMode != "debug" && cfg.GinMode != "test" {
		validationErrors = append(validationErrors, errors.New("GIN_MODE must be release, debug, or test"))
	}
	if math.IsNaN(cfg.RateLimitRPS) || math.IsInf(cfg.RateLimitRPS, 0) || cfg.RateLimitRPS <= 0 || cfg.RateLimitBurst <= 0 {
		validationErrors = append(validationErrors, errors.New("rate limit values must be positive"))
	}
	if cfg.LogQueueSize <= 0 || cfg.LogBatchSize <= 0 || cfg.LogFlushInterval <= 0 {
		validationErrors = append(validationErrors, errors.New("log queue, batch size, and flush interval must be positive"))
	}
	if cfg.RPCTimeout <= 0 || cfg.SlowRequestThreshold <= 0 || cfg.BodyCaptureLimit <= 0 || cfg.ShutdownTimeout <= 0 {
		validationErrors = append(validationErrors, errors.New("timeouts and body capture limit must be positive"))
	}
	return cfg, errors.Join(validationErrors...)
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
