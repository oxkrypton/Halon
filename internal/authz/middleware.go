package authz

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	principalContextKey  = "authz_principal"
	permissionContextKey = "authz_permission"
	deniedContextKey     = "authz_denied"
)

// AbortFunc matches both gateway.abortWithError and agent.abortWithAPIError.
type AbortFunc func(c *gin.Context, status int, code, message string)

// Middleware authenticates the Bearer credential and stores the Principal.
func Middleware(authenticator *Authenticator, abort AbortFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		scheme, credentials, found := strings.Cut(c.GetHeader("Authorization"), " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || credentials == "" {
			unauthorized(c, abort)
			return
		}
		principal, err := authenticator.Authenticate(credentials)
		if err != nil {
			unauthorized(c, abort)
			return
		}
		c.Set(principalContextKey, principal)
		c.Next()
	}
}

func unauthorized(c *gin.Context, abort AbortFunc) {
	c.Set(deniedContextKey, true)
	c.Header("WWW-Authenticate", "Bearer")
	abort(c, http.StatusUnauthorized, "UNAUTHORIZED", "valid bearer token required")
}

// Require denies a request whose principal lacks permission, or whose user
// still has to change their password. Endpoints needed to perform that change
// (such as /auth/password and /auth/me) must not be guarded by this middleware.
func Require(permission Permission, abort AbortFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(permissionContextKey, permission)
		principal := PrincipalFromContext(c)
		if principal != nil && principal.PasswordChangeRequired {
			c.Set(deniedContextKey, true)
			abort(c, http.StatusForbidden, "PASSWORD_CHANGE_REQUIRED", "password change required")
			return
		}
		if principal == nil || !principal.Role.Allows(permission) {
			c.Set(deniedContextKey, true)
			abort(c, http.StatusForbidden, "FORBIDDEN", "permission required: "+permission.String())
			return
		}
		c.Next()
	}
}

func PrincipalFromContext(c *gin.Context) *Principal {
	value, _ := c.Get(principalContextKey)
	principal, _ := value.(*Principal)
	return principal
}

func PermissionFromContext(c *gin.Context) Permission {
	value, _ := c.Get(permissionContextKey)
	permission, _ := value.(Permission)
	return permission
}

// DeniedFromContext reports whether authz Middleware or Require rejected the
// request (401/403). Downstream handler errors (e.g. 400/404) return false.
func DeniedFromContext(c *gin.Context) bool {
	denied, _ := c.Get(deniedContextKey)
	value, _ := denied.(bool)
	return value
}
