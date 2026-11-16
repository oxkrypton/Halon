package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

const requestIDKey = "request_id"

var fallbackRequestID atomic.Uint64

func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if !validRequestID(requestID) {
			requestID = newRequestID()
		}
		c.Set(requestIDKey, requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}

func validRequestID(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

func newRequestID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err == nil {
		return hex.EncodeToString(data)
	}
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), fallbackRequestID.Add(1))
}

func requestIDFromContext(c *gin.Context) string {
	value, _ := c.Get(requestIDKey)
	requestID, _ := value.(string)
	return requestID
}

func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		logger.Error("panic while handling request",
			"request_id", requestIDFromContext(c),
			"path", c.Request.URL.Path,
			"panic", recovered,
			"stack", string(debug.Stack()),
		)
		abortWithError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
	})
}

func adminAuthentication(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		scheme, credentials, found := strings.Cut(c.GetHeader("Authorization"), " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || !constantTimeEqual(credentials, token) {
			c.Header("WWW-Authenticate", "Bearer")
			abortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "valid bearer token required")
			return
		}
		c.Next()
	}
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for i := range left {
		difference |= left[i] ^ right[i]
	}
	return difference == 0
}
