package gateway

import (
	"github.com/gin-gonic/gin"
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

// upstreamTimeoutKey marks requests whose upstream call exceeded the proxy
// timeout; the access log middleware turns them into timeout incidents.
const upstreamTimeoutKey = "upstream_timeout"
