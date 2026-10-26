package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/money"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type sampleRequest struct {
	Email  string      `json:"email" validate:"required,email"`
	Amount money.Minor `json:"amount" validate:"gt=0"`
}

func (s *sampleRequest) Normalize() { s.Email = strings.ToLower(strings.TrimSpace(s.Email)) }

func decodeBody(t *testing.T, contentType, body string, optional bool) (sampleRequest, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	var dst sampleRequest
	var err error
	if optional {
		err = DecodeOptional(httptest.NewRecorder(), r, &dst)
	} else {
		err = Decode(httptest.NewRecorder(), r, &dst)
	}
	return dst, err
}

func TestDecode(t *testing.T) {
	const ct = "application/json"
	tests := []struct {
		name       string
		ct, body   string
		wantStatus int
		wantField  string
	}{
		{"valid", ct + "; charset=utf-8", `{"email":" Ada@X.dev ","amount":"100"}`, 0, ""},
		{"duplicated content type merged by a proxy", ct + ", " + ct, `{"email":"ada@x.dev","amount":100}`, 0, ""},
		{"wrong content type", "text/plain", `{}`, http.StatusUnsupportedMediaType, ""},
		{"json only as second value", "text/plain, " + ct, `{}`, http.StatusUnsupportedMediaType, ""},
		{"missing content type", "", `{}`, http.StatusUnsupportedMediaType, ""},
		{"empty body", ct, ``, http.StatusBadRequest, ""},
		{"malformed", ct, `{"email":`, http.StatusBadRequest, ""},
		{"syntax error", ct, `{"email" "x"}`, http.StatusBadRequest, ""},
		{"unknown field", ct, `{"email":"a@b.co","amount":1,"amonut":2}`, http.StatusBadRequest, "amonut"},
		{"wrong type", ct, `{"email":5,"amount":1}`, http.StatusBadRequest, "email"},
		{"decimal money", ct, `{"email":"a@b.co","amount":1.5}`, http.StatusBadRequest, "amount"},
		{"trailing data", ct, `{"email":"a@b.co","amount":1}{}`, http.StatusBadRequest, ""},
		{"array instead of object", ct, `[1,2]`, http.StatusBadRequest, ""},
		{"validation", ct, `{"email":"nope","amount":0}`, http.StatusBadRequest, "email"},
		{"too large", ct, `{"email":"` + strings.Repeat("a", MaxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeBody(t, tt.ct, tt.body, false)
			if tt.wantStatus == 0 {
				require.NoError(t, err)
				assert.Equal(t, "ada@x.dev", got.Email, "Normalize runs before validation")
				assert.Equal(t, money.Minor(100), got.Amount)
				return
			}
			ae, ok := apperr.As(err)
			require.True(t, ok, "want *apperr.Error, got %v", err)
			assert.Equal(t, tt.wantStatus, ae.Status)
			if tt.wantField != "" {
				require.NotEmpty(t, ae.Fields)
				assert.Equal(t, tt.wantField, ae.Fields[0].Field)
			}
		})
	}
}

func TestDecodeOptionalEmptyBodyStillValidates(t *testing.T) {
	_, err := decodeBody(t, "", "", true)
	ae, ok := apperr.As(err)
	require.True(t, ok)
	assert.Equal(t, apperr.CodeValidation, ae.Code, "zero values are validated even without a body")
}

func TestParsePage(t *testing.T) {
	tests := []struct {
		query       string
		page, limit int
	}{
		{"", 1, DefaultPageLimit},
		{"page=3&limit=25", 3, 25},
		{"page=0&limit=0", 1, DefaultPageLimit},
		{"page=-4&limit=-1", 1, DefaultPageLimit},
		{"page=abc&limit=xyz", 1, DefaultPageLimit},
		{"limit=100000", 1, MaxPageLimit},
		{"page=999999999", maxPage, DefaultPageLimit},
	}
	for _, tt := range tests {
		p := ParsePage(httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil))
		assert.Equal(t, tt.page, p.Page, tt.query)
		assert.Equal(t, tt.limit, p.Limit, tt.query)
	}
}

func TestNewPageMeta(t *testing.T) {
	m := NewPageMeta(21, Page{Page: 2, Limit: 10})
	assert.Equal(t, PageMeta{Total: 21, Page: 2, Limit: 10, TotalPages: 3, HasNextPage: true, HasPrevPage: true}, m)
	assert.Equal(t, 0, NewPageMeta(0, Page{Page: 1, Limit: 10}).TotalPages)
}

func TestLikePattern(t *testing.T) {
	assert.Nil(t, LikePattern("   "))
	assert.Equal(t, `%100\%\_off\\%`, *LikePattern(`100%_off\`))
	long := strings.Repeat("é", 150)
	p := *LikePattern(long)
	assert.Equal(t, 100+2, len([]rune(p)), "truncates on rune boundaries")
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 41.58.1.2")

	assert.Equal(t, "10.0.0.9", clientIP(r, 0), "XFF ignored with no trusted proxies")
	assert.Equal(t, "41.58.1.2", clientIP(r, 1), "one trusted hop: rightmost entry")
	assert.Equal(t, "6.6.6.6", clientIP(r, 2))
	assert.Equal(t, "10.0.0.9", clientIP(r, 5), "more hops than entries falls back to peer")

	r.Header.Set("X-Forwarded-For", "not-an-ip")
	assert.Equal(t, "10.0.0.9", clientIP(r, 1))
}

func TestRequestIDMiddleware(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = reqctx.RequestID(r.Context()) }))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(RequestIDHeader, "bff-trace-123")
	h.ServeHTTP(rec, r)
	assert.Equal(t, "bff-trace-123", seen)
	assert.Equal(t, "bff-trace-123", rec.Header().Get(RequestIDHeader))

	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(RequestIDHeader, "bad id with spaces\n")
	h.ServeHTTP(rec, r)
	assert.Len(t, seen, 24, "malformed ids are replaced")
}

func TestRecovererRendersJSON500(t *testing.T) {
	h := Recoverer(discard)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	var env ErrorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, apperr.CodeInternal, env.Code)
	assert.NotContains(t, rec.Body.String(), "boom", "panic details never reach the client")
}

func TestErrorHidesInternalDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, httptest.NewRequest(http.MethodGet, "/", nil), discard, errors.New("pq: password authentication failed for user irate"))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "password authentication")
}

func TestErrorRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, httptest.NewRequest(http.MethodGet, "/", nil), discard, apperr.TooManyRequests(apperr.CodeRateLimited, "slow down", 1500_000_000))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"), "rounded up to whole seconds")
}

func TestSecureHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	SecureHeaders(true)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.NotEmpty(t, rec.Header().Get("Strict-Transport-Security"))
}

func TestIsStrongPassword(t *testing.T) {
	assert.True(t, IsStrongPassword("Password123!"))
	assert.True(t, IsStrongPassword("Ñandú2024x"))
	assert.False(t, IsStrongPassword("short1A"))
	assert.False(t, IsStrongPassword("alllowercase1"))
	assert.False(t, IsStrongPassword("ALLUPPERCASE1"))
	assert.False(t, IsStrongPassword("NoDigitsHere"))
	assert.False(t, IsStrongPassword(strings.Repeat("Aa1", 43)))
}
