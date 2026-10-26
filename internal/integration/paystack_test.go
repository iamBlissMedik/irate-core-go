//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamBlissMedik/irate-core-go/internal/app"
	"github.com/iamBlissMedik/irate-core-go/internal/payment"
)

const paystackKey = "sk_test_integration_0123456789abcdef0123456789"

// paystackStub plays the Paystack API: initialize returns a checkout URL and
// verify answers from a per-reference table the test controls.
type paystackStub struct {
	mu          sync.Mutex
	initialized map[string]map[string]any // reference -> initialize body
	verify      map[string]map[string]any // reference -> verify data
	verifyCalls int
}

func startPaystackStub(t *testing.T) (*paystackStub, string) {
	t.Helper()
	s := &paystackStub{initialized: map[string]map[string]any{}, verify: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+paystackKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"status":false,"message":"Invalid key"}`)
			return
		}
		switch {
		case r.URL.Path == "/transaction/initialize":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			ref, _ := body["reference"].(string)
			s.initialized[ref] = body
			_ = json.NewEncoder(w).Encode(map[string]any{"status": true, "message": "ok",
				"data": map[string]any{"authorization_url": "https://checkout.paystack.com/" + ref, "reference": ref}})
		case strings.HasPrefix(r.URL.Path, "/transaction/verify/"):
			s.verifyCalls++
			data, ok := s.verify[strings.TrimPrefix(r.URL.Path, "/transaction/verify/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"status":false,"message":"Transaction reference not found"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": true, "message": "ok", "data": data})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func (s *paystackStub) setVerify(ref, status string, amount int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verify[ref] = map[string]any{"status": status, "amount": amount, "currency": "NGN"}
}

// sendPaystackWebhook delivers a signed charge.success event to apiURL.
func sendPaystackWebhook(t *testing.T, apiURL string, txID int64, reference string, amount int64, signer *payment.Paystack) response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"event": "charge.success", "data": map[string]any{
		"id": txID, "status": "success", "reference": reference, "amount": amount, "currency": "NGN",
	}})
	require.NoError(t, err)
	r, err := http.NewRequest(http.MethodPost, apiURL+"/api/v1/webhooks/payments/paystack", bytes.NewReader(body))
	require.NoError(t, err)
	r.Header = signer.SignForTest(body)
	res, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := response{Status: res.StatusCode, Raw: raw}
	_ = json.Unmarshal(raw, &out.Env)
	return out
}

// TestPaystackEndToEnd boots a second, fully wired app instance configured
// with PAYMENTS_PROVIDER=paystack against a stub of the Paystack API.
func TestPaystackEndToEnd(t *testing.T) {
	reset(t)
	stub, stubURL := startPaystackStub(t)

	cfg := testConfig
	cfg.Payments.Provider = payment.PaystackName
	cfg.Payments.PaystackSecretKey = paystackKey
	cfg.Payments.PaystackBaseURL = stubURL
	cfg.AutoMigrate = false
	psApp, err := app.New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(psApp.Close)
	psServer := httptest.NewServer(psApp.Handler())
	t.Cleanup(psServer.Close)
	signer := payment.NewPaystack(paystackKey, "", "", nil)

	// Users and admin are created through the shared database.
	admin := createAdmin(t, "admin@irate.test")
	ada := register(t, "ada@irate.test")

	call := func(method, path string, body any, headers map[string]string) response {
		b, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, psServer.URL+"/api/v1"+path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+ada.Token)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(r)
		require.NoError(t, err)
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		out := response{Status: res.StatusCode, Raw: raw}
		_ = json.Unmarshal(raw, &out.Env)
		return out
	}

	// 1. Start a deposit: our reference + Paystack's hosted checkout URL.
	res := call(http.MethodPost, "/payments/deposits", map[string]any{"amount": 500000}, map[string]string{"Idempotency-Key": newKey()})
	require.Equal(t, http.StatusCreated, res.Status, string(res.Raw))
	var d struct {
		ID          uuid.UUID
		Reference   string
		Provider    string
		CheckoutURL string `json:"checkoutUrl"`
	}
	require.NoError(t, json.Unmarshal(res.Env.Data, &d))
	assert.Equal(t, "paystack", d.Provider)
	assert.Equal(t, "https://checkout.paystack.com/"+d.Reference, d.CheckoutURL)
	init := stub.initialized[d.Reference]
	require.NotNil(t, init, "initialize was called with our reference")
	assert.Equal(t, "ada@irate.test", init["email"])
	assert.EqualValues(t, 500000, init["amount"])

	// 2. Webhook arrives before Paystack can verify it: 502, nothing recorded.
	wh := sendPaystackWebhook(t, psServer.URL, 1001, d.Reference, 500000, signer)
	assert.Equal(t, http.StatusBadGateway, wh.Status, string(wh.Raw))
	assert.EqualValues(t, 0, balance(t, ada))

	// 3. Paystack says the charge was NOT successful: still no credit.
	stub.setVerify(d.Reference, "abandoned", 500000)
	wh = sendPaystackWebhook(t, psServer.URL, 1001, d.Reference, 500000, signer)
	assert.Equal(t, http.StatusBadGateway, wh.Status)
	assert.EqualValues(t, 0, balance(t, ada), "a signed webhook alone never mints money")

	// 4. Verified success: the redelivered event credits exactly once.
	stub.setVerify(d.Reference, "success", 500000)
	wh = sendPaystackWebhook(t, psServer.URL, 1001, d.Reference, 500000, signer)
	require.Equal(t, http.StatusOK, wh.Status, string(wh.Raw))
	assert.EqualValues(t, 500000, balance(t, ada))
	assert.Equal(t, http.StatusOK, sendPaystackWebhook(t, psServer.URL, 1001, d.Reference, 500000, signer).Status)
	assert.EqualValues(t, 500000, balance(t, ada), "redelivery is a no-op")

	var got struct{ Status string }
	require.NoError(t, json.Unmarshal(call(http.MethodGet, "/payments/deposits/"+d.ID.String(), nil, nil).Env.Data, &got))
	assert.Equal(t, "SUCCEEDED", got.Status)

	// 5. The verified amount is authoritative: a webhook claiming 5,000.00
	//    for a charge Paystack verifies at 1.00 is flagged, not credited.
	res = call(http.MethodPost, "/payments/deposits", map[string]any{"amount": 500000}, map[string]string{"Idempotency-Key": newKey()})
	require.NoError(t, json.Unmarshal(res.Env.Data, &d))
	stub.setVerify(d.Reference, "success", 100)
	assert.Equal(t, http.StatusOK, sendPaystackWebhook(t, psServer.URL, 1002, d.Reference, 500000, signer).Status)
	require.NoError(t, json.Unmarshal(call(http.MethodGet, "/payments/deposits/"+d.ID.String(), nil, nil).Env.Data, &got))
	assert.Equal(t, "FAILED", got.Status)
	assert.EqualValues(t, 500000, balance(t, ada))

	// 6. Forged signature and sandbox simulation of a Paystack deposit.
	forger := payment.NewPaystack("sk_test_attacker", "", "", nil)
	assert.Equal(t, http.StatusUnauthorized, sendPaystackWebhook(t, psServer.URL, 1003, d.Reference, 500000, forger).Status)
	res = call(http.MethodPost, "/payments/sandbox/deposits/"+d.ID.String()+"/simulate", map[string]string{"outcome": "success"}, nil)
	assert.Equal(t, http.StatusBadRequest, res.Status, "only sandbox deposits can be simulated")

	assert.GreaterOrEqual(t, stub.verifyCalls, 4)
	assertLedgerHealthy(t, admin)
}
