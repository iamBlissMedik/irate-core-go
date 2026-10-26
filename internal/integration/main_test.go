//go:build integration

// Package integration exercises the whole service over HTTP against a real
// PostgreSQL and Redis. These tests are where the concurrency guarantees
// (row locking, idempotency, exactly-once webhooks) are actually proven.
//
//	TEST_DATABASE_URL=postgres://localhost:5432/irate_test?sslmode=disable \
//	TEST_REDIS_URL=redis://localhost:6379/15 \
//	go test -tags integration -race ./internal/integration/...
//
// WARNING: the test database is wiped before every test, and the Redis
// logical database in TEST_REDIS_URL is FLUSHed. Never point these at data
// you care about.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/iamBlissMedik/irate-core-go/internal/app"
	"github.com/iamBlissMedik/irate-core-go/internal/auth"
	"github.com/iamBlissMedik/irate-core-go/internal/config"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
)

const (
	testPassword       = "Password123!"
	testWebhookSecret  = "integration-webhook-secret-0123456789abcdef"
	unverifiedDayLimit = 5_000_000 // NGN 50,000.00
)

var (
	application *app.App
	server      *httptest.Server
	testConfig  config.Config // the configuration application was built with
)

func TestMain(m *testing.M) {
	dbURL, redisURL := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_REDIS_URL")
	if dbURL == "" || redisURL == "" {
		fmt.Println("integration tests skipped: set TEST_DATABASE_URL and TEST_REDIS_URL")
		os.Exit(0)
	}
	time.Local = time.UTC

	for k, v := range map[string]string{
		"APP_ENV":                               "test",
		"DATABASE_URL":                          dbURL,
		"REDIS_URL":                             redisURL,
		"AUTO_MIGRATE":                          "true",
		"METRICS_PORT":                          "0",
		"DB_MAX_CONNS":                          "30",
		"RATE_LIMIT_ENABLED":                    "false", // concurrency tests would trip it; covered by unit tests
		"REFRESH_REUSE_GRACE":                   "1s",
		"LOGIN_MAX_ATTEMPTS":                    "5",
		"UNVERIFIED_DAILY_TRANSFER_LIMIT_MINOR": fmt.Sprint(unverifiedDayLimit),
		"JWT_ACCESS_SECRET":                     "integration-access-secret-0123456789abcdef",
		"SANDBOX_WEBHOOK_SECRET":                testWebhookSecret,
		"LOG_LEVEL":                             "error",
	} {
		os.Setenv(k, v)
	}
	cfg, _, err := config.Load()
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}
	testConfig = cfg
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	application, err = app.New(context.Background(), cfg, log)
	if err != nil {
		fmt.Println("app:", err)
		os.Exit(1)
	}
	server = httptest.NewServer(application.Handler())

	code := m.Run()
	server.Close()
	application.Close()
	os.Exit(code)
}

// reset gives every test an empty database and Redis.
func reset(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, database.ResetData(ctx, application.Pool()))
	require.NoError(t, application.Redis().FlushDB(ctx).Err())
}

// ------------------------------------------------------------- HTTP client

type response struct {
	Status int
	Header http.Header
	Raw    []byte
	Env    struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Code    string          `json:"code"`
		Data    json.RawMessage `json:"data"`
		Errors  []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
	}
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.Env.Data, v), "body: %s", r.Raw)
}

type req struct {
	method, path, token string
	body                any
	headers             map[string]string
}

func do(t *testing.T, rq req) response {
	t.Helper()
	var body io.Reader
	if rq.body != nil {
		b, err := json.Marshal(rq.body)
		require.NoError(t, err)
		body = bytes.NewReader(b)
	}
	r, err := http.NewRequest(rq.method, server.URL+"/api/v1"+rq.path, body)
	require.NoError(t, err)
	if rq.body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if rq.token != "" {
		r.Header.Set("Authorization", "Bearer "+rq.token)
	}
	for k, v := range rq.headers {
		r.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	out := response{Status: res.StatusCode, Header: res.Header, Raw: raw}
	if len(raw) > 0 && strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		require.NoError(t, json.Unmarshal(raw, &out.Env), "body: %s", raw)
	}
	return out
}

func get(t *testing.T, token, path string) response {
	return do(t, req{method: http.MethodGet, path: path, token: token})
}

func post(t *testing.T, token, path string, body any, headers ...string) response {
	h := map[string]string{}
	for i := 0; i+1 < len(headers); i += 2 {
		h[headers[i]] = headers[i+1]
	}
	return do(t, req{method: http.MethodPost, path: path, token: token, body: body, headers: h})
}

func newKey() string { return uuid.NewString() }

// ------------------------------------------------------------- fixtures

type customer struct {
	Token, Email, Account string
	UserID, WalletID      uuid.UUID
}

func register(t *testing.T, email string) customer {
	t.Helper()
	res := post(t, "", "/auth/register", map[string]string{"email": email, "password": testPassword})
	require.Equal(t, http.StatusCreated, res.Status, string(res.Raw))
	var reg struct {
		User          struct{ ID uuid.UUID } `json:"user"`
		AccountNumber string                 `json:"accountNumber"`
	}
	res.decode(t, &reg)

	c := customer{Email: email, Account: reg.AccountNumber, UserID: reg.User.ID, Token: login(t, email, testPassword).Token}
	var wallets struct {
		Wallets []struct{ ID uuid.UUID } `json:"wallets"`
	}
	get(t, c.Token, "/wallets").decode(t, &wallets)
	require.Len(t, wallets.Wallets, 1)
	c.WalletID = wallets.Wallets[0].ID
	return c
}

type session struct {
	Token, RefreshCookie string
}

func login(t *testing.T, email, password string) session {
	t.Helper()
	res := post(t, "", "/auth/login", map[string]string{"email": email, "password": password})
	require.Equal(t, http.StatusOK, res.Status, string(res.Raw))
	var s struct {
		AccessToken string `json:"accessToken"`
	}
	res.decode(t, &s)
	return session{Token: s.AccessToken, RefreshCookie: refreshCookie(res)}
}

func refreshCookie(res response) string {
	for _, c := range (&http.Response{Header: res.Header}).Cookies() {
		if c.Name == "refreshToken" {
			return c.Value
		}
	}
	return ""
}

// createAdmin inserts an ADMIN directly (sign-ups are always USER) and logs in.
func createAdmin(t *testing.T, email string) customer {
	t.Helper()
	ctx := context.Background()
	hash, err := auth.NewHasher(2).Hash(ctx, testPassword)
	require.NoError(t, err)
	u, err := db.New(application.Pool()).CreateUser(ctx, db.CreateUserParams{
		ID: uuid.Must(uuid.NewV7()), Email: email, PasswordHash: hash, Role: db.UserRoleADMIN,
	})
	require.NoError(t, err)
	return customer{Email: email, UserID: u.ID, Token: login(t, email, testPassword).Token}
}

func fund(t *testing.T, admin customer, c customer, amount int64) {
	t.Helper()
	res := post(t, admin.Token, "/admin/credit/"+c.WalletID.String(),
		map[string]any{"amount": amount, "reason": "Test funding"}, "Idempotency-Key", newKey())
	require.Equal(t, http.StatusOK, res.Status, string(res.Raw))
}

func verifyKYC(t *testing.T, admin, c customer) {
	t.Helper()
	res := post(t, c.Token, "/kyc/submit", map[string]any{
		"fullName": "Test " + strings.Split(c.Email, "@")[0], "dateOfBirth": "1990-01-15",
		"address": "1 Marina Road, Lagos", "idType": "NIN", "idNumber": "12345678901",
	})
	require.Equal(t, http.StatusCreated, res.Status, string(res.Raw))
	var rec struct{ ID uuid.UUID }
	res.decode(t, &rec)
	res = do(t, req{method: http.MethodPatch, path: "/admin/kyc/" + rec.ID.String() + "/review", token: admin.Token, body: map[string]string{"action": "APPROVE"}})
	require.Equal(t, http.StatusOK, res.Status, string(res.Raw))
}

func balance(t *testing.T, c customer) int64 {
	t.Helper()
	var b struct {
		Balance string `json:"balance"`
	}
	res := get(t, c.Token, "/wallets/"+c.WalletID.String()+"/balance")
	require.Equal(t, http.StatusOK, res.Status, string(res.Raw))
	res.decode(t, &b)
	var n int64
	_, err := fmt.Sscan(b.Balance, &n)
	require.NoError(t, err)
	return n
}

func transfer(t *testing.T, from customer, toAccount string, amount int64, key string) response {
	return post(t, from.Token, "/transactions/transfer",
		map[string]any{"toAccountNumber": toAccount, "amount": amount}, "Idempotency-Key", key)
}

// assertLedgerHealthy runs the reconciliation endpoint: balanced journals,
// balances matching their entries, and every currency netting to zero.
func assertLedgerHealthy(t *testing.T, admin customer) {
	t.Helper()
	var rec struct {
		Healthy bool `json:"healthy"`
	}
	res := get(t, admin.Token, "/admin/reconciliation")
	require.Equal(t, http.StatusOK, res.Status)
	res.decode(t, &rec)
	require.True(t, rec.Healthy, "ledger reconciliation failed: %s", res.Raw)
}
