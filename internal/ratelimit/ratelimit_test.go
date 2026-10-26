package ratelimit

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"

	"github.com/iamBlissMedik/irate-core-go/internal/platform/metrics"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func serve(h http.Handler, ip string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	h.ServeHTTP(rec, r.WithContext(reqctx.WithClientIP(r.Context(), ip)))
	return rec
}

func TestMiddleware(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	policy := Policy{Name: "test", Limit: redis_rate.PerMinute(3)}

	t.Run("enforces the limit per key", func(t *testing.T) {
		h := New(rdb, true, metrics.New(), discard).Middleware(policy, ByIP)(ok)
		for i := range 3 {
			assert.Equal(t, http.StatusOK, serve(h, "1.1.1.1").Code, "request %d", i+1)
		}
		rec := serve(h, "1.1.1.1")
		assert.Equal(t, http.StatusTooManyRequests, rec.Code)
		assert.NotEmpty(t, rec.Header().Get("Retry-After"))
		assert.Equal(t, "0", rec.Header().Get("RateLimit-Remaining"))
		assert.Equal(t, http.StatusOK, serve(h, "2.2.2.2").Code, "other clients are unaffected")
	})

	t.Run("disabled is a passthrough", func(t *testing.T) {
		h := New(rdb, false, metrics.New(), discard).Middleware(policy, ByIP)(ok)
		for range 10 {
			assert.Equal(t, http.StatusOK, serve(h, "3.3.3.3").Code)
		}
	})

	t.Run("fails open when redis is down", func(t *testing.T) {
		down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
		h := New(down, true, metrics.New(), discard).Middleware(policy, ByIP)(ok)
		assert.Equal(t, http.StatusOK, serve(h, "4.4.4.4").Code)
	})
}

func TestByUserFallsBackToIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(reqctx.WithClientIP(r.Context(), "9.9.9.9"))
	assert.Equal(t, "ip:9.9.9.9", ByUser(r))
}
