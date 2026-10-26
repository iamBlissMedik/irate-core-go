package payment

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sandboxEvent(t *testing.T, typ string) []byte {
	t.Helper()
	p := SandboxPayload{ID: "evt_1", Type: typ}
	p.Data.Reference, p.Data.Amount, p.Data.Currency = "DEP-1", 500000, "NGN"
	b, err := json.Marshal(p)
	require.NoError(t, err)
	return b
}

func TestSandboxWebhookVerification(t *testing.T) {
	sb := NewSandbox("whsec-test-secret-xxxxxxxxxxxxxxxxxxxxxx", "http://localhost:8000")
	body := sandboxEvent(t, "payment.succeeded")

	t.Run("valid signature", func(t *testing.T) {
		ev, err := sb.ParseWebhook(sb.SignedRequestHeaders(body), body)
		require.NoError(t, err)
		assert.Equal(t, OutcomeSucceeded, ev.Outcome)
		assert.Equal(t, "DEP-1", ev.Reference)
		assert.Equal(t, int64(500000), ev.Amount)
	})
	t.Run("tampered body", func(t *testing.T) {
		h := sb.SignedRequestHeaders(body)
		tampered := []byte(string(body[:len(body)-2]) + "9}") // change the amount
		_, err := sb.ParseWebhook(h, tampered)
		assert.ErrorIs(t, err, ErrInvalidSignature)
	})
	t.Run("signed with another secret", func(t *testing.T) {
		other := NewSandbox("another-secret-yyyyyyyyyyyyyyyyyyyyyyyyy", "")
		_, err := sb.ParseWebhook(other.SignedRequestHeaders(body), body)
		assert.ErrorIs(t, err, ErrInvalidSignature)
	})
	t.Run("stale timestamp (replayed capture)", func(t *testing.T) {
		old := NewSandbox("whsec-test-secret-xxxxxxxxxxxxxxxxxxxxxx", "")
		old.now = func() time.Time { return time.Now().Add(-10 * time.Minute) }
		_, err := sb.ParseWebhook(old.SignedRequestHeaders(body), body)
		assert.ErrorIs(t, err, ErrInvalidSignature)
	})
	t.Run("missing headers", func(t *testing.T) {
		h := sb.SignedRequestHeaders(body)
		h.Del(SandboxTimestampHeader)
		_, err := sb.ParseWebhook(h, body)
		assert.ErrorIs(t, err, ErrInvalidSignature)
		h = sb.SignedRequestHeaders(body)
		h.Set(SandboxSignatureHeader, "not-hex")
		_, err = sb.ParseWebhook(h, body)
		assert.ErrorIs(t, err, ErrInvalidSignature)
	})
	t.Run("timestamp is part of the signature", func(t *testing.T) {
		h := sb.SignedRequestHeaders(body)
		ts, _ := strconv.ParseInt(h.Get(SandboxTimestampHeader), 10, 64)
		h.Set(SandboxTimestampHeader, strconv.FormatInt(ts+1, 10))
		_, err := sb.ParseWebhook(h, body)
		assert.ErrorIs(t, err, ErrInvalidSignature)
	})
	t.Run("outcome mapping", func(t *testing.T) {
		for typ, want := range map[string]EventOutcome{
			"payment.succeeded": OutcomeSucceeded,
			"payment.failed":    OutcomeFailed,
			"refund.created":    OutcomeIgnored,
		} {
			b := sandboxEvent(t, typ)
			ev, err := sb.ParseWebhook(sb.SignedRequestHeaders(b), b)
			require.NoError(t, err)
			assert.Equal(t, want, ev.Outcome, typ)
		}
	})
	t.Run("malformed but signed", func(t *testing.T) {
		b := []byte(`{"id":""}`)
		_, err := sb.ParseWebhook(sb.SignedRequestHeaders(b), b)
		assert.ErrorIs(t, err, ErrMalformedEvent)
	})
}

func TestSandboxCheckoutURLEscapesReference(t *testing.T) {
	sb := NewSandbox("s", "http://x")
	c, err := sb.InitiateDeposit(context.Background(), DepositIntent{Reference: "A&B"})
	require.NoError(t, err)
	assert.Equal(t, "http://x/sandbox/checkout?reference=A%26B", c.URL)
}
