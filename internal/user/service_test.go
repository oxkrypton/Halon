package user_test

import (
	"context"
	"testing"

	userv1 "github.com/oxkrypton/Halon/api/gen/user/v1"
	"github.com/oxkrypton/Halon/internal/user"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGetUser(t *testing.T) {
	t.Parallel()

	service := user.NewService()
	tests := []struct {
		name string
		id   string
		code codes.Code
		want string
	}{
		{name: "existing user", id: "1", code: codes.OK, want: "Alice"},
		{name: "another existing user", id: "2", code: codes.OK, want: "Bob"},
		{name: "missing user", id: "404", code: codes.NotFound},
		{name: "empty id", id: "", code: codes.InvalidArgument},
		{name: "blank id", id: "  ", code: codes.InvalidArgument},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			response, err := service.GetUser(context.Background(), &userv1.GetUserRequest{Id: tt.id})
			if got := status.Code(err); got != tt.code {
				t.Fatalf("GetUser() code = %v, want %v (err = %v)", got, tt.code, err)
			}
			if tt.code != codes.OK {
				return
			}
			if response.GetUser().GetName() != tt.want {
				t.Errorf("GetUser() name = %q, want %q", response.GetUser().GetName(), tt.want)
			}
		})
	}
}

func TestGetUserNilRequest(t *testing.T) {
	t.Parallel()

	_, err := user.NewService().GetUser(context.Background(), nil)
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("GetUser(nil) code = %v, want %v", got, codes.InvalidArgument)
	}
}
