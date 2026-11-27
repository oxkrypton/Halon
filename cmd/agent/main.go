package main

import (
	"context"
	"database/sql"
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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/oxkrypton/Halon/internal/agent"
	"github.com/oxkrypton/Halon/internal/agent/tools"
	"github.com/oxkrypton/Halon/internal/authz"
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
	if err := checkGatewayCredential(logger, cfg.Auth, cfg.GatewayAdminURL, cfg.GatewayAdminToken, cfg.OperationLevel); err != nil {
		return fmt.Errorf("gateway credential: %w", err)
	}

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

	clientset, err := kubernetesClientset()
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}
	sqlDB, err := store.SQLDB()
	if err != nil {
		return fmt.Errorf("access postgres connection pool: %w", err)
	}
	kubernetesToolset, allTools := assembleTools(sqlDB, clientset, cfg)
	registry, err := agent.NewRegistry(cfg.OperationLevel, allTools...)
	if err != nil {
		return fmt.Errorf("register agent tools: %w", err)
	}
	logger.Info("agent tools registered", "level", cfg.OperationLevel, "tools", registeredToolNames(registry))

	model := agent.NewOpenAIClient(cfg.OpenAIBaseURL, cfg.OpenAIAPIKey, cfg.OpenAIModel, cfg.OpenAITimeout)
	loop := agent.NewLoop(model, registry, cfg.MaxSteps, cfg.MaxToolOutput, cfg.MaxStoredToolOutput, logger)
	orchestrator := agent.NewOrchestrator(loop, store, agent.OrchestratorOptions{
		Level:          cfg.OperationLevel,
		Workers:        cfg.Workers,
		QueueSize:      cfg.QueueSize,
		RunTimeout:     cfg.RunTimeout,
		RecoveryMaxAge: cfg.RecoveryMaxAge,
		Metrics:        metrics,
		Logger:         logger,
	})
	orchestratorDone := make(chan struct{})
	go func() {
		orchestrator.Run()
		close(orchestratorDone)
	}()

	recoveryContext, cancelRecovery := context.WithTimeout(context.Background(), 15*time.Second)
	requeued, failed, recoveryErr := orchestrator.RecoverUnfinished(recoveryContext)
	cancelRecovery()
	if recoveryErr != nil {
		logger.Error("startup incident recovery incomplete", "requeued", requeued, "failed", failed, "error", recoveryErr)
	} else if requeued > 0 || failed > 0 {
		logger.Info("startup incident recovery completed", "requeued", requeued, "failed", failed)
	}

	// In jwt mode INGEST_TOKEN is only a webhook credential; it must not
	// grant report reads through the static-token fallback.
	staticToken := cfg.IngestToken
	if cfg.Auth.Mode == authz.ModeJWT {
		staticToken = ""
	}
	authenticator, err := authz.NewAuthenticator(cfg.Auth, authz.AudienceAgent, staticToken)
	if err != nil {
		return fmt.Errorf("configure agent authentication: %w", err)
	}
	logger.Info("agent authentication mode", "mode", cfg.Auth.Mode)

	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: agent.NewHTTPHandler(agent.ServerDeps{
			Orchestrator: orchestrator,
			Deduper:      incident.NewDeduper(cfg.DedupTTL),
			Reports:      store,
			Pods:         kubernetesToolset,
			IngestToken:  cfg.IngestToken,
			Auth:         authenticator,
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

	// Drain queued sessions; abort any remaining active runs if ShutdownTimeout expires.
	go orchestrator.Close()
	select {
	case <-orchestratorDone:
	case <-shutdownContext.Done():
		orchestrator.CancelActive()
		<-orchestratorDone
		shutdownErrors = append(shutdownErrors, errors.New("orchestrator drain timed out"))
	}
	return errors.Join(append(shutdownErrors, runError)...)
}

// checkGatewayCredential binds the agent's gateway identity to its operation
// level. Static tokens cannot be scoped, so a lower role gets a warning in
// legacy/hybrid mode and is rejected outright in jwt mode.
func checkGatewayCredential(logger *slog.Logger, config authz.Config, gatewayAdminURL, token string, level agent.OperationLevel) error {
	// With a gateway admin URL configured, operator/full agents will make
	// write calls at runtime. An empty credential would only surface as a
	// 401 on the first tool call, so fail startup instead. Read-only agents
	// and an unconfigured gateway URL keep the historical behaviour.
	if token == "" && gatewayAdminURL != "" && level.Allows(agent.LevelOperator) {
		return errors.New("GATEWAY_ADMIN_TOKEN is required when GATEWAY_ADMIN_URL is set and AGENT_OPERATION_LEVEL is operator or full")
	}
	if !authz.LooksLikeJWT(token) {
		if config.Mode == authz.ModeJWT {
			if token == "" && (level == agent.LevelReadOnly || gatewayAdminURL == "") {
				return nil
			}
			return errors.New("GATEWAY_ADMIN_TOKEN must be a signed JWT in jwt mode")
		}
		if token != "" && level != agent.LevelFull {
			logger.Warn("static GATEWAY_ADMIN_TOKEN grants full gateway access; operation level is lower",
				"operation_level", level, "auth_mode", config.Mode)
		}
		return nil
	}
	verifier, err := config.Verifier()
	if err != nil {
		return err
	}
	if verifier == nil {
		return errors.New("JWT GATEWAY_ADMIN_TOKEN requires AUTH_MODE hybrid or jwt")
	}
	principal, err := verifier.Verify(token, authz.AudienceGatewayAdmin)
	if err != nil {
		return fmt.Errorf("verify GATEWAY_ADMIN_TOKEN: %w", err)
	}
	// The startup self-check is only for agent credentials: a human token
	// must never be reused as the agent's gateway identity.
	if principal.Type != authz.TypeAgent {
		return fmt.Errorf("GATEWAY_ADMIN_TOKEN typ %q must be %q", principal.Type, authz.TypeAgent)
	}
	if principal.Role.String() != string(level) {
		return fmt.Errorf("GATEWAY_ADMIN_TOKEN role %q does not match AGENT_OPERATION_LEVEL %q", principal.Role, level)
	}
	return nil
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

func assembleTools(sqlDB *sql.DB, clientset kubernetes.Interface, cfg agent.Config) (*tools.KubernetesToolset, []agent.Tool) {
	cooldownStore := tools.NewMemoryCooldownStore()
	kubernetesOptions := tools.KubernetesOptions{
		AllowedNamespaces: cfg.K8sNamespaces,
		MaxReplicas:       cfg.K8sMaxReplicas,
		ActionCooldown:    cfg.K8sActionCooldown,
		Cooldowns:         cooldownStore,
	}
	kubernetesToolset, kubernetesTools := tools.NewKubernetesTools(clientset, kubernetesOptions)
	allTools := append(tools.ObservabilityTools(sqlDB, cfg.PrometheusURL, nil), kubernetesTools...)
	allTools = append(allTools, tools.NewGatewayTools(kubernetesToolset, cfg.GatewayAdminURL, cfg.GatewayAdminToken, nil)...)
	return kubernetesToolset, allTools
}

func registeredToolNames(registry *agent.Registry) []string {
	definitions := registry.Definitions()
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Function.Name)
	}
	return names
}
