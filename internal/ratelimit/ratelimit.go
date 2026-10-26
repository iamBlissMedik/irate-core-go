// Package ratelimit provides Redis-backed rate limiting that holds across
// every API instance (an in-memory limiter would multiply the limit by the
// number of replicas).
//
// It uses GCRA (the generic cell rate algorithm, via go-redis/redis_rate):
// smooth limiting with no burst at window boundaries, one atomic Lua call per
// request.
package ratelimit

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/metrics"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

// Policy is a named limit.
type Policy struct {
	Name  string
	Limit redis_rate.Limit
}

// Default policies. Credential endpoints are limited per IP (the caller is
// anonymous); everything else per authenticated user, which also works
// behind the BFF, where every request arrives from the same server IP.
var (
	PolicyAuth     = Policy{Name: "auth", Limit: redis_rate.PerMinute(20)}
	PolicyUser     = Policy{Name: "user", Limit: redis_rate.PerMinute(300)}
	PolicyTransfer = Policy{Name: "transfer", Limit: redis_rate.PerMinute(20)}
	// Name enquiry is limited tightly: it maps account numbers to names, so
	// unlimited access would let someone scrape the customer directory.
	PolicyEnquiry = Policy{Name: "enquiry", Limit: redis_rate.PerMinute(30)}
)

// KeyFunc picks the identity a limit applies to.
type KeyFunc func(r *http.Request) string

// ByIP limits per resolved client IP.
func ByIP(r *http.Request) string { return "ip:" + reqctx.ClientIP(r.Context()) }

// ByUser limits per authenticated user, falling back to IP.
func ByUser(r *http.Request) string {
	if p, ok := reqctx.PrincipalFrom(r.Context()); ok {
		return "user:" + p.UserID.String()
	}
	return ByIP(r)
}

type Limiter struct {
	limiter *redis_rate.Limiter
	enabled bool
	metrics *metrics.Metrics
	log     *slog.Logger
}

func New(rdb *redis.Client, enabled bool, m *metrics.Metrics, log *slog.Logger) *Limiter {
	return &Limiter{limiter: redis_rate.NewLimiter(rdb), enabled: enabled, metrics: m, log: log}
}

// Middleware enforces policy per key. It sets the IETF draft RateLimit-*
// headers and Retry-After on 429.
//
// If Redis is unavailable it FAILS OPEN: rate limiting protects capacity, and
// rejecting all traffic because the limiter is down would cause the very
// outage it exists to prevent. (Authentication, by contrast, fails closed.)
func (l *Limiter) Middleware(policy Policy, key KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if !l.enabled {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			res, err := l.limiter.Allow(r.Context(), "ratelimit:"+policy.Name+":"+key(r), policy.Limit)
			if err != nil {
				l.log.WarnContext(r.Context(), "rate limiter unavailable; allowing request", "err", err, "policy", policy.Name)
				l.metrics.DependencyFailOpen.WithLabelValues("rate_limit").Inc()
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("RateLimit-Limit", strconv.Itoa(policy.Limit.Rate))
			h.Set("RateLimit-Remaining", strconv.Itoa(res.Remaining))
			h.Set("RateLimit-Reset", strconv.Itoa(seconds(res.ResetAfter)))
			if res.Allowed == 0 {
				httpx.Error(w, r, l.log, apperr.TooManyRequests(apperr.CodeRateLimited,
					"Too many requests. Please slow down and try again shortly.", res.RetryAfter))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func seconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Seconds()))
}
