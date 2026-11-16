package gateway

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

const (
	limiterCleanupInterval = time.Minute
	limiterIdleTimeout     = 10 * time.Minute
)

type clientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type RateLimiter struct {
	mu        sync.Mutex
	clients   map[string]*clientLimiter
	rps       float64
	burst     int
	lastPurge time.Time
}

func NewRateLimiter(rps float64, burst int) *RateLimiter {
	return &RateLimiter{
		clients:   make(map[string]*clientLimiter),
		rps:       rps,
		burst:     burst,
		lastPurge: time.Now(),
	}
}

func (l *RateLimiter) Allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastPurge) >= limiterCleanupInterval {
		for key, client := range l.clients {
			if now.Sub(client.lastSeen) >= limiterIdleTimeout {
				delete(l.clients, key)
			}
		}
		l.lastPurge = now
	}
	client, ok := l.clients[ip]
	if !ok {
		client = &clientLimiter{limiter: rate.NewLimiter(rate.Limit(l.rps), l.burst)}
		l.clients[ip] = client
	}
	client.lastSeen = now
	return client.limiter.AllowN(now, 1)
}

func (l *RateLimiter) Update(rps float64, burst int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rps = rps
	l.burst = burst
	for _, client := range l.clients {
		client.limiter.SetLimit(rate.Limit(rps))
		client.limiter.SetBurst(burst)
	}
}

func (l *RateLimiter) Settings() (float64, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rps, l.burst
}

// Middleware applies the per-IP rate limit. Each rejection invokes the
// optional hooks (used for metrics) before the error response is written.
func (l *RateLimiter) Middleware(onRejected ...func()) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !l.Allow(c.ClientIP()) {
			for _, hook := range onRejected {
				hook()
			}
			c.Header("Retry-After", "1")
			abortWithError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
			return
		}
		c.Next()
	}
}
