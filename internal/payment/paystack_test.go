package payment

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPaystackKey = "sk_test_0123456789abcdef0123456789abcdef01234567"

// fakePaystack mimics the two Paystack endpoints the adapter uses.
type fakePaystack struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []map[string]any
	verify   map[string]map[string]any // reference -> data
}

func newFakePaystack(t *testing.T) (*fakePaystack, *httptest.Server) {
	t.Helper()
	f := &fakePaystack{verify: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &body)
		}
		f.requests, f.bodies = append(f.requests, r), append(f.bodies, body)

		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+testPaystackKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":false,"message":"Invalid key"}`))
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/transaction/initialize":
			if body["reference"] == "DEP-DUPLICATE" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":false,"message":"Duplicate Transaction Reference"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": true, "message": "Authorization URL created",
				"data": map[string]any{"authorization_url": "https://checkout.paystack.com/abc123", "reference": body["reference"]}})
		case r.Method == http.MethodGet && len(r.URL.Path) > len("/transaction/verify/"):
			ref := r.URL.Path[len("/transaction/verify/"):]
			data, ok := f.verify[ref]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"status":false,"message":"Transaction reference not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": true, "message": "Verification successful", "data": data})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestPaystackInitiateDeposit(t *testing.T) {
	f, srv := newFakePaystack(t)
	p := NewPaystack(testPaystackKey, srv.URL, "https://app.irate.dev/deposits/done", nil)

	c, err := p.InitiateDeposit(context.Background(), DepositIntent{Reference: "DEP-1", Amount: 500000, Currency: "NGN", CustomerEmail: "ada@irate.dev"})
	require.NoError(t, err)
	assert.Equal(t, "https://checkout.paystack.com/abc123", c.URL)

	require.Len(t, f.bodies, 1)
	assert.Equal(t, "application/json", f.requests[0].Header.Get("Content-Type"))
	assert.Equal(t, map[string]any{
		"email": "ada@irate.dev", "amount": float64(500000), "currency": "NGN", "reference": "DEP-1",
		"callback_url": "https://app.irate.dev/deposits/done",
	}, f.bodies[0], "amount is sent in kobo, unchanged")

	_, err = p.InitiateDeposit(context.Background(), DepositIntent{Reference: "DEP-DUPLICATE", Amount: 1, Currency: "NGN"})
	assert.ErrorContains(t, err, "Duplicate Transaction Reference", "provider error messages are surfaced for logs")

	_, err = NewPaystack("sk_test_wrong", srv.URL, "", nil).InitiateDeposit(context.Background(), DepositIntent{Reference: "x"})
	assert.ErrorContains(t, err, "HTTP 401")
}

func TestPaystackVerifyDeposit(t *testing.T) {
	f, srv := newFakePaystack(t)
	p := NewPaystack(testPaystackKey, srv.URL, "", nil)
	f.verify["DEP-OK"] = map[string]any{"status": "success", "amount": 250000, "currency": "ngn"}
	f.verify["DEP/ODD REF"] = map[string]any{"status": "abandoned", "amount": 250000, "currency": "NGN"}

	v, err := p.VerifyDeposit(context.Background(), "DEP-OK")
	require.NoError(t, err)
	assert.Equal(t, Verification{Succeeded: true, Amount: 250000, Currency: "NGN"}, v)

	v, err = p.VerifyDeposit(context.Background(), "DEP/ODD REF")
	require.NoError(t, err)
	assert.False(t, v.Succeeded)
	assert.Equal(t, "/transaction/verify/DEP%2FODD%20REF", f.requests[len(f.requests)-1].URL.RawPath, "reference is path-escaped")

	_, err = p.VerifyDeposit(context.Background(), "DEP-UNKNOWN")
	assert.ErrorContains(t, err, "not found")
}

func paystackEvent(t *testing.T, event, status string, id int64) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"event": event, "data": map[string]any{
		"id": id, "status": status, "reference": "DEP-1", "amount": 500000, "currency": "NGN", "gateway_response": "Approved",
	}})
	require.NoError(t, err)
	return b
}

func TestPaystackParseWebhook(t *testing.T) {
	p := NewPaystack(testPaystackKey, "", "", nil)

	body := paystackEvent(t, "charge.success", "success", 302961)
	ev, err := p.ParseWebhook(p.SignForTest(body), body)
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, ev.Outcome)
	assert.Equal(t, "charge.success:302961", ev.ID)
	assert.Equal(t, "DEP-1", ev.Reference)
	assert.Equal(t, int64(500000), ev.Amount)

	t.Run("tampered body", func(t *testing.T) {
		h := p.SignForTest(body)
		_, err := p.ParseWebhook(h, paystackEvent(t, "charge.success", "success", 999))
		assert.ErrorIs(t, err, ErrInvalidSignature)
	})
	t.Run("signed with another key", func(t *testing.T) {
		other := NewPaystack("sk_test_attacker", "", "", nil)
		_, err := p.ParseWebhook(other.SignForTest(body), body)
		assert.ErrorIs(t, err, ErrInvalidSignature)
	})
	t.Run("missing signature", func(t *testing.T) {
		_, err := p.ParseWebhook(http.Header{}, body)
		assert.ErrorIs(t, err, ErrInvalidSignature)
	})
	t.Run("non-success events are acknowledged and ignored", func(t *testing.T) {
		for _, b := range [][]byte{
			paystackEvent(t, "transfer.success", "success", 1),
			paystackEvent(t, "refund.processed", "processed", 2),
			paystackEvent(t, "charge.success", "failed", 3), // defensive: inconsistent payload
		} {
			ev, err := p.ParseWebhook(p.SignForTest(b), b)
			require.NoError(t, err)
			assert.Equal(t, OutcomeIgnored, ev.Outcome, string(b))
		}
	})
	t.Run("malformed but signed", func(t *testing.T) {
		b := []byte(`{"event":"charge.success","data":{}}`)
		_, err := p.ParseWebhook(p.SignForTest(b), b)
		assert.ErrorIs(t, err, ErrMalformedEvent)
	})
	t.Run("event id falls back to reference", func(t *testing.T) {
		b := paystackEvent(t, "charge.success", "success", 0)
		ev, err := p.ParseWebhook(p.SignForTest(b), b)
		require.NoError(t, err)
		assert.Equal(t, "charge.success:DEP-1", ev.ID)
	})
}

func TestPaystackImplementsVerifier(t *testing.T) {
	var p Provider = NewPaystack(testPaystackKey, "", "", nil)
	_, ok := p.(Verifier)
	assert.True(t, ok)
	_, ok = Provider(NewSandbox("s", "")).(Verifier)
	assert.False(t, ok, "the sandbox has no verify endpoint")
}
