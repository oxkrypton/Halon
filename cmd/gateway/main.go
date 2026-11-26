package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/gateway"
	"github.com/oxkrypton/Halon/internal/identity"
	"github.com/oxkrypton/Halon/internal/incident"
	"github.com/oxkrypton/Halon/internal/trafficlog"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := gateway.LoadConfig()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	slog.SetDefault(logger)
	gin.SetMode(cfg.GinMode)

	startupContext, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	repository, err := trafficlog.OpenPostgres(startupContext, cfg.DatabaseDSN)
	cancelStartup()
	if err != nil {
		return err
	}
	defer func() { _ = repository.Close() }()
	if err := repository.Migrate(); err != nil {
		return err
	}

	// Local login is opt-in. Identity uses its own migration version table,
	// so it can share the database with trafficlog safely.
	var identityStore *identity.Store
	var cleaner *identity.Cleaner
	cleanerDone := make(chan struct{})
	if cfg.LoginEnabled {
		identityContext, cancelIdentity := context.WithTimeout(context.Background(), 15*time.Second)
		identityStore, err = identity.OpenStore(identityContext, cfg.DatabaseDSN)
		cancelIdentity()
		if err != nil {
			return fmt.Errorf("open identity store: %w", err)
		}
		defer func() { _ = identityStore.Close() }()
		if err := identityStore.Migrate(); err != nil {
			return fmt.Errorf("apply identity migrations: %w", err)
		}
		if cfg.BootstrapAdminUser != "" {
			bootstrapContext, cancelBootstrap := context.WithTimeout(context.Background(), 15*time.Second)
			created, err := identityStore.BootstrapAdmin(bootstrapContext,
				cfg.BootstrapAdminUser, cfg.BootstrapAdminPassword,
				identity.PasswordPolicy{MinLength: cfg.PasswordMinLength})
			cancelBootstrap()
			if err != nil {
				return fmt.Errorf("bootstrap administrator: %w", err)
			}
			if created {
				// One line only: the password is never logged.
				logger.Info("bootstrap administrator created", "username", cfg.BootstrapAdminUser)
			}
		}
		cleaner = identity.NewCleaner(identityStore, cfg.AuthEventRetention, logger)
		go func() {
			cleaner.Run(context.Background())
			close(cleanerDone)
		}()
	} else {
		close(cleanerDone)
	}
	// Also covers startup failures that happen after the cleaner was started.
	defer func() {
		if cleaner != nil {
			cleaner.Close()
		}
	}()

	tracing, err := gateway.InitTracing(context.Background(), gateway.TracingConfig{
		Enabled:      cfg.OTELEnabled,
		Endpoint:     cfg.OTELExporterEndpoint,
		SamplerRatio: cfg.OTELTracesSamplerRatio,
	})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracing.Shutdown(ctx); err != nil {
			logger.Error("shutdown tracer provider", "error", err)
		}
	}()

	routes, err := gateway.ParseRoutes(cfg.Routes)
	if err != nil {
		return fmt.Errorf("parse routes: %w", err)
	}
	logger.Info("proxy routes loaded", "count", len(routes.Routes()))

	metrics := gateway.NewMetrics()

	// Redis backs blacklist and rate limiting across gateway replicas. Startup
	// ping failure does not abort startup; stores degrade per policy.
	options, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}
	options.DialTimeout = cfg.RedisDialTimeout
	options.ReadTimeout = cfg.RedisOpTimeout
	options.WriteTimeout = cfg.RedisOpTimeout
	redisClient := redis.NewClient(options)
	defer func() { _ = redisClient.Close() }()
	pingContext, cancelPing := context.WithTimeout(context.Background(), cfg.RedisDialTimeout)
	pingErr := redisClient.Ping(pingContext).Err()
	cancelPing()
	metrics.SetRedisUp(pingErr == nil)
	if pingErr != nil {
		logger.Warn("redis unreachable at startup; running in degraded mode", "error", pingErr)
	}

	rules := gateway.NewRedisBlacklistStore(redisClient, cfg.BlacklistCacheTTL, cfg.RedisOpTimeout, logger, metrics.SetRedisUp)
	defer rules.Close()
	limiter := gateway.NewRedisRateLimiter(redisClient, cfg.RateLimitRPS, cfg.RateLimitBurst, cfg.RedisOpTimeout, logger, metrics.SetRedisUp)

	queue := make(chan trafficlog.Entry, cfg.LogQueueSize)
	writer := trafficlog.NewBatchWriter(repository, queue, cfg.LogBatchSize, cfg.LogFlushInterval, logger)
	writerDone := make(chan struct{})
	go func() {
		writer.Run()
		close(writerDone)
	}()

	var timeoutQueue chan incident.TimeoutEvent
	notifierDone := make(chan struct{})
	if cfg.AgentWebhookURL != "" {
		timeoutQueue = make(chan incident.TimeoutEvent, cfg.TimeoutQueueSize)
		notifier := gateway.NewTimeoutNotifier(cfg.AgentWebhookURL, cfg.AgentWebhookToken, timeoutQueue, logger, metrics)
		go func() {
			notifier.Run()
			close(notifierDone)
		}()
	} else {
		close(notifierDone)
	}

	proxyOptions := gateway.ProxyOptions{
		Timeout:               cfg.ProxyTimeout,
		DialTimeout:           cfg.UpstreamDialTimeout,
		TLSHandshakeTimeout:   cfg.UpstreamTLSHandshakeTimeout,
		ResponseHeaderTimeout: cfg.UpstreamResponseHeaderTimeout,
		MaxConnsPerHost:       cfg.UpstreamMaxConnsPerHost,
		MaxRequestBodyBytes:   cfg.MaxRequestBodyBytes,
		RetryBodyBufferBytes:  cfg.RetryBodyBufferBytes,
		Resilience: gateway.ResilienceConfig{
			RetryMaxAttempts:         cfg.RetryMaxAttempts,
			BreakerEnabled:           cfg.BreakerEnabled,
			BreakerFailureThreshold:  cfg.BreakerFailureThreshold,
			BreakerOpenDuration:      cfg.BreakerOpenDuration,
			BreakerHalfOpenSuccesses: cfg.BreakerHalfOpenSuccesses,
			BreakerCount5xx:          cfg.BreakerCount5xx,
			ConcurrencyPerRoute:      cfg.ConcurrencyPerRoute,
			Logger:                   logger,
		},
		Logger:           logger,
		TransportWrapper: tracing.WrapTransport,
	}
	resilience := proxyOptions.BuildResilienceSet(routes.Prefixes(), metrics)

	httpServer := newHTTPServer(cfg.HTTPAddr, tracing.WrapHandler(gateway.NewHTTPHandler(gateway.Dependencies{
		Routes:               routes,
		Repository:           repository,
		LogQueue:             queue,
		TimeoutQueue:         timeoutQueue,
		Logger:               logger,
		Rules:                rules,
		Limiter:              limiter,
		Metrics:              metrics,
		Proxy:                proxyOptions,
		Resilience:           resilience,
		SlowRequestThreshold: cfg.SlowRequestThreshold,
		BodyCaptureLimit:     cfg.BodyCaptureLimit,
	})))
	authenticator, err := authz.NewAuthenticator(cfg.Auth, authz.AudienceGatewayAdmin, cfg.AdminToken)
	if err != nil {
		return fmt.Errorf("configure authentication: %w", err)
	}
	logger.Info("admin authentication mode", "mode", cfg.Auth.Mode)

	var identityService *identity.Service
	var authService gateway.AuthService
	var userService gateway.UserService
	var loginLimiter gateway.RateLimitBackend
	if cfg.LoginEnabled {
		privateKey, err := authz.ParsePrivateKeyBase64(cfg.JWTPrivateKey)
		if err != nil {
			return fmt.Errorf("parse AUTH_JWT_PRIVATE_KEY: %w", err)
		}
		signer, err := authz.NewSigner(privateKey, cfg.JWTKID, cfg.Auth.Issuer)
		if err != nil {
			return fmt.Errorf("configure login signer: %w", err)
		}
		identityService, err = identity.NewService(identityStore, signer, identity.ServiceConfig{
			AccessTokenTTL:     cfg.AccessTokenTTL,
			RefreshTokenTTL:    cfg.RefreshTokenTTL,
			RefreshMaxLifetime: cfg.RefreshMaxLifetime,
			Audiences:          []authz.Audience{authz.AudienceGatewayAdmin, authz.AudienceAgent},
			Issuer:             cfg.Auth.Issuer,
			PasswordPolicy:     identity.PasswordPolicy{MinLength: cfg.PasswordMinLength},
			MaxFailures:        cfg.LoginMaxFailures,
			LockDuration:       cfg.LoginLockDuration,
		}, logger)
		if err != nil {
			return fmt.Errorf("configure identity service: %w", err)
		}
		authService = identityService
		userService = identityService
		loginLimiter = gateway.NewRedisRateLimiter(redisClient,
			cfg.LoginRateLimitRPS, cfg.LoginRateLimitBurst, cfg.RedisOpTimeout,
			logger, metrics.SetRedisUp,
			gateway.WithRateLimitKeyPrefix("halon:ratelimit:login:"))
		logger.Info("local login enabled", "kid", cfg.JWTKID)
	}

	adminServer := newHTTPServer(cfg.AdminAddr, gateway.NewAdminHandler(cfg.AdminToken, rules, limiter, logger, metrics, gateway.AdminOptions{
		MetricsRequireAuth: cfg.MetricsRequireAuth,
		Resilience:         resilience,
		Auth:               authenticator,
		LoginEnabled:       cfg.LoginEnabled,
		AuthService:        authService,
		UserService:        userService,
		LoginLimiter:       loginLimiter,
	}))

	serverErrors := make(chan error, 2)
	go listen(httpServer, "http", serverErrors)
	go listen(adminServer, "admin", serverErrors)

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var runError error
	select {
	case <-signalContext.Done():
		logger.Info("shutting down gateway")
	case err := <-serverErrors:
		runError = err
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelShutdown()
	shutdownErrors := make(chan error, 2)
	for _, server := range []*http.Server{httpServer, adminServer} {
		go func(server *http.Server) {
			shutdownErrors <- server.Shutdown(shutdownContext)
		}(server)
	}
	var shutdownResult []error
	for range 2 {
		if err := <-shutdownErrors; err != nil {
			shutdownResult = append(shutdownResult, err)
		}
	}

	// Stop accepting events only after all in-flight HTTP handlers have finished.
	close(queue)
	if timeoutQueue != nil {
		close(timeoutQueue)
	}
	select {
	case <-writerDone:
	case <-shutdownContext.Done():
		shutdownResult = append(shutdownResult, errors.New("traffic log writer shutdown timed out"))
	}
	select {
	case <-notifierDone:
	case <-shutdownContext.Done():
		shutdownResult = append(shutdownResult, errors.New("timeout notifier shutdown timed out"))
	}
	if cleaner != nil {
		cleaner.Close()
		select {
		case <-cleanerDone:
		case <-shutdownContext.Done():
			shutdownResult = append(shutdownResult, errors.New("auth data cleaner shutdown timed out"))
		}
	}
	return errors.Join(append(shutdownResult, runError)...)
}

func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

func listen(server *http.Server, name string, errorsChannel chan<- error) {
	slog.Info("gateway listener starting", "listener", name, "address", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errorsChannel <- fmt.Errorf("%s listener: %w", name, err)
	}
}
