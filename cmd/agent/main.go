package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/oxkrypton/Halon/internal/agent"
	"github.com/oxkrypton/Halon/internal/agent/tools"
	"github.com/oxkrypton/Halon/internal/incident"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := agent.LoadConfig()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	gin.SetMode(cfg.GinMode)

	startupContext, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	store, err := incident.OpenStore(startupContext, cfg.DatabaseDSN)
	cancelStartup()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(); err != nil {
		return err
	}

	metrics := agent.NewMetrics()

	// Optional shared-state mode: with REDIS_URL set, action cooldowns are
	// arbitrated in Redis so multiple replicas can run. Ping failure does
	// not abort startup; mutation tools reject actions while degraded.
	sharedStateMode := "memory"
	var redisClient *redis.Client
	if cfg.RedisURL != "" {
		options, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("parse REDIS_URL: %w", err)
		}
		options.DialTimeout = cfg.RedisDialTimeout
		options.ReadTimeout = cfg.RedisOpTimeout
		options.WriteTimeout = cfg.RedisOpTimeout
		redisClient = redis.NewClient(options)
		defer func() { _ = redisClient.Close() }()
		pingContext, cancelPing := context.WithTimeout(context.Background(), cfg.RedisDialTimeout)
		pingErr := redisClient.Ping(pingContext).Err()
		cancelPing()
		metrics.SetRedisUp(pingErr == nil)
		if pingErr != nil {
			logger.Warn("redis unreachable at startup; running with degraded shared state", "error", pingErr)
		}
		sharedStateMode = "redis"
	}
	logger.Info("shared state mode", "shared_state_mode", sharedStateMode)

	clientset, err := kubernetesClientset()
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}
	kubernetesOptions := tools.KubernetesOptions{
		AllowedNamespaces: cfg.K8sNamespaces,
		MaxReplicas:       cfg.K8sMaxReplicas,
		ActionCooldown:    cfg.K8sActionCooldown,
	}
	if redisClient != nil {
		kubernetesOptions.Cooldowns = tools.NewRedisCooldownStore(redisClient, cfg.RedisOpTimeout, logger, metrics.SetRedisUp)
	}
	kubernetesToolset, kubernetesTools := tools.NewKubernetesTools(clientset, kubernetesOptions)
	sqlDB, err := store.SQLDB()
	if err != nil {
		return fmt.Errorf("access postgres connection pool: %w", err)
	}
	allTools := append(tools.ObservabilityTools(sqlDB, cfg.PrometheusURL, nil), kubernetesTools...)
	allTools = append(allTools, tools.NewGatewayActionTool(kubernetesToolset, cfg.GatewayAdminURL, cfg.GatewayAdminToken, nil))
	registry, err := agent.NewRegistry(cfg.OperationLevel, allTools...)
	if err != nil {
		return fmt.Errorf("register agent tools: %w", err)
	}

	model := agent.NewOpenAIClient(cfg.OpenAIBaseURL, cfg.OpenAIAPIKey, cfg.OpenAIModel, cfg.OpenAITimeout)
	loop := agent.NewLoop(model, registry, cfg.MaxSteps, cfg.MaxToolOutput, cfg.MaxStoredToolOutput, logger)
	var routeSuppressor agent.RouteSuppressor
	if cfg.CoalesceCooldown > 0 && redisClient != nil {
		routeSuppressor = agent.NewRedisRouteSuppressor(redisClient, cfg.RedisOpTimeout, logger, metrics.SetRedisUp)
	}
	orchestrator := agent.NewOrchestrator(loop, store, agent.OrchestratorOptions{
		Level:            cfg.OperationLevel,
		Workers:          cfg.Workers,
		QueueSize:        cfg.QueueSize,
		RunTimeout:       cfg.RunTimeout,
		RecoveryMaxAge:   cfg.RecoveryMaxAge,
		CoalesceCooldown: cfg.CoalesceCooldown,
		RetryBackoff:     cfg.RetryBackoff,
		SettleDelay:      cfg.SettleDelay,
		Suppressor:       routeSuppressor,
		Logger:           logger,
	})
	orchestratorDone := make(chan struct{})
	go func() {
		orchestrator.Run()
		close(orchestratorDone)
	}()

	// Startup recovery (memory mode only): rows still queued/processing were
	// claimed by a previous process whose in-memory queue died with it. A
	// single replica makes them unambiguous orphans, so re-queueing is safe.
	// In shared mode a live replica may own those rows, so recovery is
	// skipped until lease-based claiming exists.
	if cfg.RedisURL == "" {
		requeued, expired, err := orchestrator.RecoverUnfinished(context.Background())
		switch {
		case err != nil:
			logger.Error("incident recovery incomplete; remaining orphans retry on next restart",
				"requeued", requeued, "expired", expired, "error", err)
		case requeued > 0 || expired > 0:
			logger.Info("incident recovery finished", "requeued", requeued, "expired", expired)
		}
	} else {
		logger.Info("startup incident recovery skipped in shared mode")
	}

	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: agent.NewHTTPHandler(agent.ServerDeps{
			Orchestrator: orchestrator,
			Deduper:      incident.NewDeduper(cfg.DedupTTL),
			Reports:      store,
			IngestToken:  cfg.IngestToken,
			Logger:       logger,
			Metrics:      metrics,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("agent listener starting", "address", server.Addr, "level", cfg.OperationLevel)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var runError error
	select {
	case <-signalContext.Done():
		logger.Info("shutting down agent")
	case err := <-serverErrors:
		runError = err
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelShutdown()
	var shutdownErrors []error
	if err := server.Shutdown(shutdownContext); err != nil {
		shutdownErrors = append(shutdownErrors, fmt.Errorf("http server shutdown: %w", err))
	}

	// Drain queued investigations; each in-flight run is bounded by RunTimeout.
	orchestrator.Close()
	select {
	case <-orchestratorDone:
	case <-shutdownContext.Done():
		shutdownErrors = append(shutdownErrors, errors.New("orchestrator drain timed out"))
	}
	return errors.Join(append(shutdownErrors, runError)...)
}

// kubernetesClientset prefers the in-cluster configuration and falls back
// to the local kubeconfig for development outside the cluster.
func kubernetesClientset() (kubernetes.Interface, error) {
	if config, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(config)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory for kubeconfig: %w", err)
	}
	config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(home, ".kube", "config"))
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(config)
}
