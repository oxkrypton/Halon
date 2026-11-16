package gateway

import (
	"errors"
	"net"
	"net/http"
	"sort"
	"sync"

	"github.com/gin-gonic/gin"
)

// BlacklistStore is the contract behind blacklist checks and admin writes.
// The single-replica memory mode uses IPRules; the shared multi-replica mode
// uses RedisBlacklistStore. Both honor the same semantics: Add/Remove report
// whether the entry actually changed, and store failures are surfaced as
// ErrBlacklistUnavailable so callers never pretend a write succeeded.
type BlacklistStore interface {
	Add(ip string) (created bool, err error)
	Remove(ip string) (removed bool, err error)
	Contains(ip string) bool
	List() []string
}

// ErrBlacklistUnavailable marks blacklist writes that failed because the
// backing store is unreachable. The admin API maps it to 503: a security
// feature must never appear to have succeeded when it has not.
var ErrBlacklistUnavailable = errors.New("blacklist store unavailable")

// BlacklistMiddleware blocks listed IPs. Each rejection invokes the optional
// hooks (used for metrics) before the error response is written.
func BlacklistMiddleware(store BlacklistStore, onRejected ...func()) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store.Contains(c.ClientIP()) {
			for _, hook := range onRejected {
				hook()
			}
			abortWithError(c, http.StatusForbidden, "IP_BLOCKED", "client IP is blocked")
			return
		}
		c.Next()
	}
}

type IPRules struct {
	mu      sync.RWMutex
	blocked map[string]struct{}
}

func NewIPRules(initial []string) (*IPRules, error) {
	rules := &IPRules{blocked: make(map[string]struct{}, len(initial))}
	for _, ip := range initial {
		if _, err := rules.Add(ip); err != nil {
			return nil, err
		}
	}
	return rules, nil
}

func canonicalIP(value string) (string, error) {
	parsed := net.ParseIP(value)
	if parsed == nil {
		return "", errors.New("invalid IP address")
	}
	return parsed.String(), nil
}

func (r *IPRules) Add(value string) (bool, error) {
	ip, err := canonicalIP(value)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.blocked[ip]
	r.blocked[ip] = struct{}{}
	return !existed, nil
}

func (r *IPRules) Remove(value string) (bool, error) {
	ip, err := canonicalIP(value)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.blocked[ip]; !ok {
		return false, nil
	}
	delete(r.blocked, ip)
	return true, nil
}

func (r *IPRules) Contains(value string) bool {
	ip, err := canonicalIP(value)
	if err != nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, blocked := r.blocked[ip]
	return blocked
}

func (r *IPRules) List() []string {
	r.mu.RLock()
	ips := make([]string, 0, len(r.blocked))
	for ip := range r.blocked {
		ips = append(ips, ip)
	}
	r.mu.RUnlock()
	sort.Strings(ips)
	return ips
}
