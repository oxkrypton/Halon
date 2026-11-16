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
	userv1 "github.com/oxkrypton/Halon/api/gen/user/v1"
	"github.com/oxkrypton/Halon/internal/gateway"
	"github.com/oxkrypton/Halon/internal/trafficlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
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
	defer repository.Close()
	if err := repository.Migrate(); err != nil {
		return err
	}

	connection, err := grpc.NewClient(cfg.UserServiceAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("create user service connection: %w", err)
	}
	defer connection.Close()

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

	httpServer := newHTTPServer(cfg.HTTPAddr, gateway.NewHTTPHandler(gateway.Dependencies{
		UserClient:           userv1.NewUserServiceClient(connection),
		HealthClient:         healthpb.NewHealthClient(connection),
		Repository:           repository,
		LogQueue:             queue,
		Logger:               logger,
		Rules:                rules,
		Limiter:              limiter,
		RPCTimeout:           cfg.RPCTimeout,
		SlowRequestThreshold: cfg.SlowRequestThreshold,
		BodyCaptureLimit:     cfg.BodyCaptureLimit,
	}))
	adminServer := newHTTPServer(cfg.AdminAddr, gateway.NewAdminHandler(cfg.AdminToken, rules, limiter, logger))

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

	// Stop accepting logs only after all in-flight HTTP handlers have finished.
	close(queue)
	select {
	case <-writerDone:
	case <-shutdownContext.Done():
		shutdownResult = append(shutdownResult, errors.New("traffic log writer shutdown timed out"))
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
