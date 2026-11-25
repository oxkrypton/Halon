package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/identity"
)

// UserService is the management contract exposed by identity.Service. The
// narrow interface keeps the HTTP layer testable without a database.
type UserService interface {
	ListUsers(ctx context.Context, offset, limit int) ([]identity.UserView, int64, error)
	GetUser(ctx context.Context, id string) (*identity.UserView, error)
	CreateUser(ctx context.Context, input identity.CreateUserInput) (*identity.CreateUserResult, error)
	UpdateUser(ctx context.Context, targetID, actorID, actorUsername string, update identity.UserUpdate, clientIP, requestID string) (*identity.UserView, error)
	ResetPassword(ctx context.Context, targetID, actorUsername, clientIP, requestID string) (string, error)
}

var _ UserService = (*identity.Service)(nil)

// userRoutesDisabled mirrors the auth routes when local login is off.
func userRoutesDisabled() []struct{ Method, Path string } {
	return []struct{ Method, Path string }{
		{http.MethodGet, "/admin/users"},
		{http.MethodPost, "/admin/users"},
		{http.MethodGet, "/admin/users/:id"},
		{http.MethodPatch, "/admin/users/:id"},
		{http.MethodPost, "/admin/users/:id/password"},
	}
}

func registerUserRoutes(router *gin.Engine, authenticator *authz.Authenticator, service UserService, logger *slog.Logger, metrics *Metrics) {
	users := router.Group("/admin/users",
		auditAdminRequest(logger, metrics),
		authz.Middleware(authenticator, abortWithError),
	)
	users.GET("", authz.Require(authz.PermAuthUserRead, abortWithError), listUsersHandler(service))
	users.GET("/:id", authz.Require(authz.PermAuthUserRead, abortWithError), getUserHandler(service))
	users.POST("", authz.Require(authz.PermAuthUserManage, abortWithError), createUserHandler(service, logger))
	users.PATCH("/:id", authz.Require(authz.PermAuthUserManage, abortWithError), updateUserHandler(service, logger))
	users.POST("/:id/password", authz.Require(authz.PermAuthUserManage, abortWithError), resetPasswordHandler(service, logger))
}

func registerUserDisabledRoutes(router *gin.Engine) {
	for _, route := range userRoutesDisabled() {
		router.Handle(route.Method, route.Path, func(c *gin.Context) {
			abortWithError(c, http.StatusNotFound, "LOGIN_DISABLED", "local login is disabled")
		})
	}
}

func listUsersHandler(service UserService) gin.HandlerFunc {
	return func(c *gin.Context) {
		offset, err := strconv.Atoi(c.DefaultQuery("offset", "0"))
		if err != nil || offset < 0 {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "offset must be a non-negative integer")
			return
		}
		limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
		if err != nil || limit <= 0 {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "limit must be a positive integer")
			return
		}
		if limit > 200 {
			limit = 200
		}
		users, total, err := service.ListUsers(c.Request.Context(), offset, limit)
		if err != nil {
			abortWithError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			return
		}
		c.JSON(http.StatusOK, gin.H{"users": users, "total": total, "offset": offset, "limit": limit})
	}
}

func getUserHandler(service UserService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := identity.ValidateUUID(c.Param("id")); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "user id must be a valid UUID")
			return
		}
		user, err := service.GetUser(c.Request.Context(), c.Param("id"))
		if err != nil {
			if errors.Is(err, identity.ErrNotFound) {
				abortWithError(c, http.StatusNotFound, "USER_NOT_FOUND", "user not found")
				return
			}
			abortWithError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			return
		}
		c.JSON(http.StatusOK, gin.H{"user": user})
	}
}

func createUserHandler(service UserService, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request struct {
			Username    string `json:"username" binding:"required"`
			Role        string `json:"role" binding:"required"`
			DisplayName string `json:"display_name"`
			Password    string `json:"password"`
		}
		if err := bindAuthJSON(c, &request); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "username and role are required")
			return
		}
		if err := identity.ValidateUsername(request.Username); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if len(request.DisplayName) > 128 {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "display_name exceeds 128 bytes")
			return
		}
		role, err := authz.ParseRole(request.Role)
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "role must be readonly, operator or full")
			return
		}
		principal := authz.PrincipalFromContext(c)
		var actorUserID *string
		if principal != nil && principal.UserID != "" {
			actorUserID = &principal.UserID
		}
		result, err := service.CreateUser(c.Request.Context(), identity.CreateUserInput{
			Username:      request.Username,
			Role:          role,
			DisplayName:   request.DisplayName,
			Password:      request.Password,
			ActorUserID:   actorUserID,
			ActorUsername: actorUsername(principal),
			ClientIP:      c.ClientIP(),
			RequestID:     requestIDFromContext(c),
		})
		if err != nil {
			switch {
			case errors.Is(err, identity.ErrUsernameTaken):
				abortWithError(c, http.StatusConflict, "USERNAME_TAKEN", "username is already taken")
			case errors.Is(err, identity.ErrWeakPassword):
				abortWithError(c, http.StatusBadRequest, "WEAK_PASSWORD", err.Error())
			case errors.Is(err, identity.ErrInvalidUsername):
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			default:
				logAuthError(logger, "user_create", c, err)
				abortWithError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			}
			return
		}
		body := gin.H{"user": result.User}
		if result.GeneratedPassword != "" {
			body["generated_password"] = result.GeneratedPassword
		}
		c.JSON(http.StatusCreated, body)
	}
}

func updateUserHandler(service UserService, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := identity.ValidateUUID(c.Param("id")); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "user id must be a valid UUID")
			return
		}
		var request struct {
			Role        *string `json:"role"`
			Disabled    *bool   `json:"disabled"`
			DisplayName *string `json:"display_name"`
		}
		if err := bindAuthJSON(c, &request); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body")
			return
		}
		update := identity.UserUpdate{}
		if request.Role != nil {
			role, err := authz.ParseRole(*request.Role)
			if err != nil {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "role must be readonly, operator or full")
				return
			}
			update.Role = &role
		}
		if request.Disabled != nil {
			update.Disabled = request.Disabled
		}
		if request.DisplayName != nil {
			if len(*request.DisplayName) > 128 {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "display_name exceeds 128 bytes")
				return
			}
			update.DisplayName = request.DisplayName
		}
		if update.Role == nil && update.Disabled == nil && update.DisplayName == nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "role, disabled or display_name is required")
			return
		}
		principal := authz.PrincipalFromContext(c)
		actorID, actorName := "", ""
		if principal != nil {
			actorID, actorName = principal.UserID, principal.Subject
		}
		user, err := service.UpdateUser(c.Request.Context(), c.Param("id"), actorID, actorName,
			update, c.ClientIP(), requestIDFromContext(c))
		if err != nil {
			switch {
			case errors.Is(err, identity.ErrCannotModifySelf):
				abortWithError(c, http.StatusForbidden, "FORBIDDEN", "cannot change own role or disable self")
			case errors.Is(err, identity.ErrLastAdmin):
				abortWithError(c, http.StatusConflict, "LAST_ADMIN", "cannot demote or disable the last administrator")
			case errors.Is(err, identity.ErrNotFound):
				abortWithError(c, http.StatusNotFound, "USER_NOT_FOUND", "user not found")
			default:
				logAuthError(logger, "user_update", c, err)
				abortWithError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			}
			return
		}
		c.JSON(http.StatusOK, gin.H{"user": user})
	}
}

func resetPasswordHandler(service UserService, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := identity.ValidateUUID(c.Param("id")); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "user id must be a valid UUID")
			return
		}
		principal := authz.PrincipalFromContext(c)
		actorName := actorUsername(principal)
		password, err := service.ResetPassword(c.Request.Context(), c.Param("id"), actorName,
			c.ClientIP(), requestIDFromContext(c))
		if err != nil {
			switch {
			case errors.Is(err, identity.ErrNotFound):
				abortWithError(c, http.StatusNotFound, "USER_NOT_FOUND", "user not found")
			case errors.Is(err, identity.ErrWeakPassword):
				abortWithError(c, http.StatusBadRequest, "WEAK_PASSWORD", err.Error())
			default:
				logAuthError(logger, "user_password_reset", c, err)
				abortWithError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			}
			return
		}
		c.JSON(http.StatusOK, gin.H{"generated_password": password, "must_change_password": true})
	}
}

func actorUsername(principal *authz.Principal) string {
	if principal == nil {
		return ""
	}
	return principal.Subject
}
