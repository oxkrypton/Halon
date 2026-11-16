package user

import (
	"context"
	"strings"
	"sync"

	userv1 "github.com/oxkrypton/Halon/api/gen/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements the user gRPC API with an in-memory repository.
// The repository is deliberately simple in this phase and can be replaced by
// PostgreSQL without changing the public protobuf contract.
type Service struct {
	userv1.UnimplementedUserServiceServer

	mu    sync.RWMutex
	users map[string]*userv1.User
}

func NewService() *Service {
	return &Service{
		users: map[string]*userv1.User{
			"1": {Id: "1", Name: "Alice", Email: "alice@example.com"},
			"2": {Id: "2", Name: "Bob", Email: "bob@example.com"},
		},
	}
}

func (s *Service) GetUser(_ context.Context, req *userv1.GetUserRequest) (*userv1.GetUserResponse, error) {
	if req == nil || strings.TrimSpace(req.GetId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "user id is required")
	}

	s.mu.RLock()
	user, ok := s.users[req.GetId()]
	s.mu.RUnlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "user %q not found", req.GetId())
	}

	return &userv1.GetUserResponse{User: user}, nil
}
