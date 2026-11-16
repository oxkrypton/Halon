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
	"github.com/oxkrypton/Halon/internal/gateway"
	"github.com/oxkrypton/Halon/internal/incident"
	"github.com/oxkrypton/Halon/internal/trafficlog"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := gateway.LoadConfig()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
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

	routes, err := gateway.ParseRoutes(cfg.Routes)
	if err != nil {
		return fmt.Errorf("parse routes: %w", err)
	}
	logger.Info("proxy routes loaded", "count", len(routes.Routes()))

	rules, err := gateway.NewIPRules(nil)
	if err != nil {
		return err
	}
	limiter := gateway.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
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
		notifier := gateway.NewTimeoutNotifier(cfg.AgentWebhookURL, cfg.AgentWebhookToken, timeoutQueue, logger)
		go func() {
			notifier.Run()
			close(notifierDone)
		}()
	} else {
		close(notifierDone)
	}
	metrics := gateway.NewMetrics()
	httpServer := newHTTPServer(cfg.HTTPAddr, gateway.NewHTTPHandler(gateway.Dependencies{
		Routes:               routes,
		Repository:           repository,
		LogQueue:             queue,
		TimeoutQueue:         timeoutQueue,
		Logger:               logger,
		Rules:                rules,
		Limiter:              limiter,
		Metrics:              metrics,
		ProxyTimeout:         cfg.ProxyTimeout,
		SlowRequestThreshold: cfg.SlowRequestThreshold,
		BodyCaptureLimit:     cfg.BodyCaptureLimit,
	}))
	adminServer := newHTTPServer(cfg.AdminAddr, gateway.NewAdminHandler(cfg.AdminToken, rules, limiter, logger, metrics))

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
