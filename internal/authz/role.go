// Package authz defines the shared RBAC model used by the gateway admin API,
// the agent API and the agent tool registry.
package authz

import (
	"fmt"
	"sort"
	"strings"
)

// Role is the single role vocabulary shared by human administrators and
// agents. Higher-ranked roles inherit the permissions of lower ones.
type Role string

const (
	RoleReadOnly Role = "readonly"
	RoleOperator Role = "operator"
	RoleFull     Role = "full"
)

// Permission is a resource:action pair referenced by both HTTP routes and
// agent tools.
type Permission string

const (
	PermIncidentRead             Permission = "incident:read"
	PermGatewayRulesRead         Permission = "gateway:rules:read"
	PermGatewayBreakerRead       Permission = "gateway:breaker:read"
	PermGatewayMetricsRead       Permission = "gateway:metrics:read"
	PermGatewayObservabilityRead Permission = "gateway:observability:read"
	PermGatewayBlacklistWrite    Permission = "gateway:blacklist:write"
	PermGatewayRateLimitWrite    Permission = "gateway:ratelimit:write"
	PermToolObserve              Permission = "tool:observe"
	PermToolK8sRestartPod        Permission = "tool:k8s:restart_pod"
	PermToolK8sScale             Permission = "tool:k8s:scale"
	PermToolK8sUpdateImage       Permission = "tool:k8s:update_image"
	PermAuthUserRead             Permission = "auth:user:read"
	PermAuthUserManage           Permission = "auth:user:manage"
)

var permissionRoles = map[Permission]Role{
	PermIncidentRead:             RoleReadOnly,
	PermGatewayRulesRead:         RoleReadOnly,
	PermGatewayBreakerRead:       RoleReadOnly,
	PermGatewayMetricsRead:       RoleReadOnly,
	PermGatewayObservabilityRead: RoleReadOnly,
	PermToolObserve:              RoleReadOnly,
	PermGatewayBlacklistWrite:    RoleOperator,
	PermToolK8sRestartPod:        RoleOperator,
	PermToolK8sScale:             RoleOperator,
	PermGatewayRateLimitWrite:    RoleFull,
	PermToolK8sUpdateImage:       RoleFull,
	PermAuthUserRead:             RoleFull,
	PermAuthUserManage:           RoleFull,
}

func ParseRole(value string) (Role, error) {
	role := Role(strings.ToLower(strings.TrimSpace(value)))
	switch role {
	case RoleReadOnly, RoleOperator, RoleFull:
		return role, nil
	default:
		return "", fmt.Errorf("unsupported role %q", value)
	}
}

func (r Role) Rank() int {
	switch r {
	case RoleFull:
		return 2
	case RoleOperator:
		return 1
	default:
		return 0
	}
}

func (r Role) Allows(permission Permission) bool {
	required, ok := permissionRoles[permission]
	return ok && r.Rank() >= required.Rank()
}

func (r Role) String() string { return string(r) }

func ParsePermission(value string) (Permission, error) {
	permission := Permission(strings.TrimSpace(value))
	if _, ok := permissionRoles[permission]; !ok {
		return "", fmt.Errorf("unsupported permission %q", value)
	}
	return permission, nil
}

// RequiredRole reports the lowest role that grants p.
func (p Permission) RequiredRole() (Role, bool) {
	role, ok := permissionRoles[p]
	return role, ok
}

func (p Permission) String() string { return string(p) }

// Permissions returns all known permissions in stable order.
func Permissions() []Permission {
	result := make([]Permission, 0, len(permissionRoles))
	for permission := range permissionRoles {
		result = append(result, permission)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
