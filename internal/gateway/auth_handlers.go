package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/identity"
)

// maxAuthBodyBytes bounds the JSON body of auth and user-management requests.
// Credentials and user data never need more than a few kilobytes.
const maxAuthBodyBytes = 8 << 10

// AuthService is the contract the gateway HTTP layer needs from identity.
// It is deliberately narrow so handlers can be exercised with httptest and a
// stub without a database.
type AuthService interface {
	Login(ctx context.Context, username, password, clientIP, userAgent, requestID string) (*identity.Session, error)
	Refresh(ctx context.Context, token, clientIP, userAgent, requestID string) (*identity.Session, error)
	Logout(ctx context.Context, token, requestID string) error
	ChangePassword(ctx context.Context, userID, currentPassword, newPassword, clientIP, requestID string) error
}

var _ AuthService = (*identity.Service)(nil)

// loginRoutesDisabled is used when AUTH_LOGIN_ENABLED=false (the default).
// The paths exist but always answer 404 LOGIN_DISABLED, which makes the
// disabled state unambiguous instead of a generic route-not-found.
func loginRoutesDisabled() []struct{ Method, Path string } {
	return []struct{ Method, Path string }{
		{http.MethodPost, "/auth/login"},
		{http.MethodPost, "/auth/refresh"},
		{http.MethodPost, "/auth/logout"},
		{http.MethodGet, "/auth/me"},
		{http.MethodPost, "/auth/password"},
	}
}

func registerAuthRoutes(router *gin.Engine, authenticator *authz.Authenticator, service AuthService, limiter RateLimitBackend, logger *slog.Logger) {
	auth := router.Group("/auth")
	auth.POST("/login", loginHandler(service, limiter, logger))
	auth.POST("/refresh", refreshHandler(service, logger))
	// Logout is keyed by the refresh token itself and needs no access token.
	auth.POST("/logout", logoutHandler(service, logger))
	// A token with pcr=true must be able to reach these two endpoints, so
	// they carry only Middleware and never Require.
	auth.GET("/me", authz.Middleware(authenticator, abortWithError), meHandler())
	auth.POST("/password", authz.Middleware(authenticator, abortWithError), changePasswordHandler(service, logger))
}

func registerAuthDisabledRoutes(router *gin.Engine) {
	for _, route := range loginRoutesDisabled() {
		router.Handle(route.Method, route.Path, func(c *gin.Context) {
			abortWithError(c, http.StatusNotFound, "LOGIN_DISABLED", "local login is disabled")
		})
	}
}

func loginHandler(service AuthService, limiter RateLimitBackend, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request struct {
			Username string `json:"username" binding:"required"`
			Password string `json:"password" binding:"required"`
		}
		if err := bindAuthJSON(c, &request); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "username and password are required")
			return
		}
		// Proxy headers are not trusted, so this is only an auxiliary guard;
		// per-account failure counters are the real backstop.
		if limiter != nil && !limiter.Allow(c.ClientIP()) {
			c.Header("Retry-After", "1")
			abortWithError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many login attempts")
			return
		}
		session, err := service.Login(c.Request.Context(), request.Username, request.Password,
			c.ClientIP(), c.GetHeader("User-Agent"), requestIDFromContext(c))
		if err != nil {
			if errors.Is(err, identity.ErrInvalidCredentials) {
				logAuth(logger, "login", c, "invalid_credentials")
				abortWithError(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid username or password")
				return
			}
			logAuthError(logger, "login", c, err)
			abortWithError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			return
		}
		logAuth(logger, "login", c, "success")
		c.JSON(http.StatusOK, session)
	}
}

func refreshHandler(service AuthService, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request struct {
			RefreshToken string `json:"refresh_token" binding:"required"`
		}
		if err := bindAuthJSON(c, &request); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "refresh_token is required")
			return
		}
		session, err := service.Refresh(c.Request.Context(), request.RefreshToken,
			c.ClientIP(), c.GetHeader("User-Agent"), requestIDFromContext(c))
		if err != nil {
			if errors.Is(err, identity.ErrInvalidRefreshToken) {
				logAuth(logger, "refresh", c, "invalid_token")
				abortWithError(c, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "invalid or expired refresh token")
				return
			}
			logAuthError(logger, "refresh", c, err)
			abortWithError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			return
		}
		logAuth(logger, "refresh", c, "success")
		c.JSON(http.StatusOK, session)
	}
}

func logoutHandler(service AuthService, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request struct {
			RefreshToken string `json:"refresh_token" binding:"required"`
		}
		if err := bindAuthJSON(c, &request); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "refresh_token is required")
			return
		}
		if err := service.Logout(c.Request.Context(), request.RefreshToken, requestIDFromContext(c)); err != nil {
			logAuthError(logger, "logout", c, err)
			abortWithError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			return
		}
		logAuth(logger, "logout", c, "success")
		c.Status(http.StatusNoContent)
	}
}

func meHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal := authz.PrincipalFromContext(c)
		if principal == nil {
			abortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "valid bearer token required")
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"username":             principal.Subject,
			"user_id":              principal.UserID,
			"role":                 principal.Role,
			"must_change_password": principal.PasswordChangeRequired,
		})
	}
}

func changePasswordHandler(service AuthService, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal := authz.PrincipalFromContext(c)
		if principal == nil || principal.UserID == "" {
			abortWithError(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "a session token is required to change a password")
			return
		}
		var request struct {
			CurrentPassword string `json:"current_password" binding:"required"`
			NewPassword     string `json:"new_password" binding:"required"`
		}
		if err := bindAuthJSON(c, &request); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "current_password and new_password are required")
			return
		}
		err := service.ChangePassword(c.Request.Context(), principal.UserID,
			request.CurrentPassword, request.NewPassword, c.ClientIP(), requestIDFromContext(c))
		if err != nil {
			switch {
			case errors.Is(err, identity.ErrWeakPassword):
				logAuth(logger, "password_change", c, "weak_password")
				abortWithError(c, http.StatusBadRequest, "WEAK_PASSWORD", err.Error())
			case errors.Is(err, identity.ErrInvalidCredentials):
				logAuth(logger, "password_change", c, "invalid_credentials")
				abortWithError(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid current password")
			default:
				logAuthError(logger, "password_change", c, err)
				abortWithError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			}
			return
		}
		logAuth(logger, "password_change", c, "success")
		c.Status(http.StatusNoContent)
	}
}

// bindAuthJSON applies a hard body limit before decoding. It never logs the
// decoded values: credentials and tokens must stay out of logs.
func bindAuthJSON(c *gin.Context, target any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAuthBodyBytes)
	return c.ShouldBindJSON(target)
}

func logAuth(logger *slog.Logger, action string, c *gin.Context, result string) {
	if logger == nil {
		return
	}
	logger.Info("auth request",
		"action", action,
		"result", result,
		"client_ip", c.ClientIP(),
		"request_id", requestIDFromContext(c),
	)
}

func logAuthError(logger *slog.Logger, action string, c *gin.Context, err error) {
	if logger == nil {
		return
	}
	logger.Error("auth request failed",
		"action", action,
		"client_ip", c.ClientIP(),
		"request_id", requestIDFromContext(c),
		"error", err,
	)
}
