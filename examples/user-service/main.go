// Command user-service is a demo backend for the Halon gateway agent.
// It is NOT part of the product: it exists so the gateway → agent incident
// loop can be exercised locally or in a playground cluster.
//
// Fault injection query parameters (demo only, never use in production):
//
//	GET /api/v1/users/1?delay=5000   sleep 5s before responding (triggers gateway timeouts)
//	GET /api/v1/users/1?status=500   force an HTTP status code
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

var users = map[string]map[string]string{
	"1": {"id": "1", "name": "Alice", "email": "alice@example.com"},
	"2": {"id": "2", "name": "Bob", "email": "bob@example.com"},
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("demo user service stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	address := os.Getenv("HTTP_ADDR")
	if address == "" {
		address = ":8090"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/users/{id}", getUser)

	server := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: the demo intentionally supports ?delay= fault injection.
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("demo user service listening", "address", address)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-signalContext.Done():
		logger.Info("shutting down demo user service")
	case err := <-serverErrors:
		return err
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdownContext)
}

func getUser(w http.ResponseWriter, r *http.Request) {
	if delay := r.URL.Query().Get("delay"); delay != "" {
		milliseconds, err := strconv.Atoi(delay)
		if err != nil || milliseconds < 0 || milliseconds > 30000 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "INVALID_DELAY", "message": "delay must be 0..30000 milliseconds"}})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Duration(milliseconds) * time.Millisecond):
		}
	}
	if forced := r.URL.Query().Get("status"); forced != "" {
		code, err := strconv.Atoi(forced)
		if err != nil || code < 100 || code > 599 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "INVALID_STATUS", "message": "status must be 100..599"}})
			return
		}
		writeJSON(w, code, map[string]any{"error": map[string]string{"code": "INJECTED", "message": "fault injection via ?status="}})
		return
	}

	id := r.PathValue("id")
	user, ok := users[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "NOT_FOUND", "message": "user not found"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
