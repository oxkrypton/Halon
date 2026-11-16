package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	userv1 "github.com/oxkrypton/Halon/api/gen/user/v1"
	"github.com/oxkrypton/Halon/internal/user"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

const defaultAddress = ":50051"

func main() {
	if err := run(); err != nil {
		slog.Error("user service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	address := os.Getenv("GRPC_ADDR")
	if address == "" {
		address = defaultAddress
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&userv1.UserService_ServiceDesc, user.NewService())

	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus(userv1.UserService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	grpcServer.RegisterService(&healthpb.Health_ServiceDesc, healthServer)
	reflection.Register(grpcServer)

	serveErrors := make(chan error, 1)
	go func() {
		slog.Info("user service listening", "address", listener.Addr().String())
		serveErrors <- grpcServer.Serve(listener)
	}()

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErrors:
		return err
	case <-signalContext.Done():
		slog.Info("shutting down user service")
	}

	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-stopped:
		return nil
	case <-timer.C:
		grpcServer.Stop()
		return errors.New("graceful shutdown timed out")
	}
}
