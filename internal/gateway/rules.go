package gateway

import (
	"errors"
	"net"
	"net/http"
	"sort"
	"sync"

	"github.com/gin-gonic/gin"
)

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

// Middleware blocks listed IPs. Each rejection invokes the optional hooks
// (used for metrics) before the error response is written.
func (r *IPRules) Middleware(onRejected ...func()) gin.HandlerFunc {
	return func(c *gin.Context) {
		if r.Contains(c.ClientIP()) {
			for _, hook := range onRejected {
				hook()
			}
			abortWithError(c, http.StatusForbidden, "IP_BLOCKED", "client IP is blocked")
			return
		}
		c.Next()
	}
}
