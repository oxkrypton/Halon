package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	userv1 "github.com/oxkrypton/Halon/api/gen/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	address := flag.String("addr", "localhost:50051", "gRPC server address")
	userID := flag.String("id", "1", "user ID to query")
	flag.Parse()

	if err := call(*address, *userID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func call(address, userID string) error {
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("create gRPC client: %w", err)
	}
	defer connection.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	healthResponse, err := healthpb.NewHealthClient(connection).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return fmt.Errorf("check server health: %w", err)
	}

	response, err := userv1.NewUserServiceClient(connection).GetUser(ctx, &userv1.GetUserRequest{Id: userID})
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}

	result := struct {
		Health string       `json:"health"`
		User   *userv1.User `json:"user"`
	}{Health: healthResponse.GetStatus().String(), User: response.GetUser()}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
