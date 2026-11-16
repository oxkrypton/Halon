package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	userv1 "github.com/oxkrypton/Halon/api/gen/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func abortWithError(c *gin.Context, httpStatus int, code, message string) {
	c.AbortWithStatusJSON(httpStatus, errorResponse{Error: errorDetail{
		Code:      code,
		Message:   message,
		RequestID: requestIDFromContext(c),
	}})
}

const upstreamTimeoutKey = "upstream_timeout"

type UserHandler struct {
	client  userv1.UserServiceClient
	timeout time.Duration
}

func (h *UserHandler) GetUser(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		abortWithError(c, http.StatusBadRequest, "INVALID_ARGUMENT", "user id is required")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	response, err := h.client.GetUser(ctx, &userv1.GetUserRequest{Id: id})
	if err != nil {
		handleUserRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": response.GetUser()})
}

func handleUserRPCError(c *gin.Context, err error) {
	rpcStatus := status.Convert(err)
	switch rpcStatus.Code() {
	case codes.InvalidArgument:
		abortWithError(c, http.StatusBadRequest, "INVALID_ARGUMENT", rpcStatus.Message())
	case codes.NotFound:
		abortWithError(c, http.StatusNotFound, "NOT_FOUND", rpcStatus.Message())
	case codes.DeadlineExceeded:
		c.Set(upstreamTimeoutKey, true)
		abortWithError(c, http.StatusGatewayTimeout, "UPSTREAM_TIMEOUT", "user service request timed out")
	case codes.Unavailable:
		abortWithError(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "user service is unavailable")
	default:
		abortWithError(c, http.StatusBadGateway, "UPSTREAM_ERROR", "user service request failed")
	}
}
