package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/metrics"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

const RequestIDHeader = "X-Request-ID"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestID propagates a caller-supplied X-Request-ID when it is well-formed
// (so a trace can span the BFF and this API), otherwise generates one. The id
// is echoed in the response header and in every error body.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(reqctx.WithRequestID(r.Context(), id)))
	})
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ClientInfo resolves the client IP and records the User-Agent.
//
// X-Forwarded-For is trivially spoofable, so it is only trusted for exactly
// `trustedHops` proxies we control (e.g. 1 behind a single load balancer):
// the client is the entry that many positions from the right. With 0 hops the
// TCP peer address is used. Blindly taking the left-most XFF entry (what many
// "RealIP" middlewares do) would let anyone dodge per-IP rate limits.
func ClientInfo(trustedHops int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := reqctx.WithClientIP(r.Context(), clientIP(r, trustedHops))
			ctx = reqctx.WithUserAgent(ctx, truncate(r.UserAgent(), 255))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func clientIP(r *http.Request, trustedHops int) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if trustedHops <= 0 {
		return peer
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(h, ",") {
			if p := strings.TrimSpace(part); p != "" {
				hops = append(hops, p)
			}
		}
	}
	if idx := len(hops) - trustedHops; idx >= 0 && idx < len(hops) {
		if ip := net.ParseIP(hops[idx]); ip != nil {
			return ip.String()
		}
	}
	return peer
}

// Recoverer turns a panic into a logged 500 instead of a dropped connection.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rec) // let net/http abort the response as intended
					}
					log.ErrorContext(r.Context(), "panic recovered",
						"panic", fmt.Sprint(rec), "stack", string(debug.Stack()),
						"request_id", reqctx.RequestID(r.Context()))
					Error(w, r, log, apperr.New(http.StatusInternalServerError, apperr.CodeInternal,
						"Something went wrong on our side. Please try again."))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog writes one structured line per request and records Prometheus
// metrics. The route label is the chi pattern ("/wallets/{walletId}/balance"),
// not the raw path, to keep metric cardinality bounded.
func AccessLog(log *slog.Logger, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			route := "unmatched"
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				route = rc.RoutePattern()
			}
			elapsed := time.Since(start)

			if m != nil {
				m.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(status)).Inc()
				m.HTTPDuration.WithLabelValues(r.Method, route).Observe(elapsed.Seconds())
			}
			if isProbe(r.URL.Path) {
				return
			}

			level := slog.LevelInfo
			switch {
			case status >= 500:
				level = slog.LevelError
			case status >= 400:
				level = slog.LevelWarn
			}
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", route),
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", elapsed),
				slog.String("ip", reqctx.ClientIP(r.Context())),
				slog.String("request_id", reqctx.RequestID(r.Context())),
			}
			if p, ok := reqctx.PrincipalFrom(r.Context()); ok {
				attrs = append(attrs, slog.String("user_id", p.UserID.String()))
			}
			log.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/health" || path == "/"
}

// SecureHeaders sets defensive headers suitable for a JSON API (the Go
// equivalent of helmet). Cache-Control: no-store matters for a fintech API:
// balances and statements must never be cached by browsers or proxies.
func SecureHeaders(hsts bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-site")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			h.Set("Cache-Control", "no-store")
			if hsts {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds the request context so slow database calls are cancelled
// instead of piling up. (http.TimeoutHandler is avoided on purpose: it buffers
// whole responses and races with handlers that keep writing.)
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
