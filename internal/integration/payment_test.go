//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamBlissMedik/irate-core-go/internal/payment"
)

type deposit struct {
	ID            uuid.UUID
	Reference     string
	Status        string
	Amount        string
	FailureReason *string
	TransactionID *uuid.UUID
}

func startDeposit(t *testing.T, c customer, amount int64, key string) (deposit, response) {
	t.Helper()
	res := post(t, c.Token, "/payments/deposits", map[string]any{"amount": amount}, "Idempotency-Key", key)
	var d deposit
	if res.Status < 300 {
		res.decode(t, &d)
	}
	return d, res
}

// sendWebhook delivers a correctly signed sandbox event, like the provider would.
func sendWebhook(t *testing.T, eventID, typ, reference string, amount int64) int {
	t.Helper()
	p := payment.SandboxPayload{ID: eventID, Type: typ}
	p.Data.Reference, p.Data.Amount, p.Data.Currency = reference, amount, "NGN"
	body, err := json.Marshal(p)
	require.NoError(t, err)

	r, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/webhooks/payments/sandbox", bytes.NewReader(body))
	require.NoError(t, err)
	r.Header = payment.NewSandbox(testWebhookSecret, "").SignedRequestHeaders(body)
	res, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}

func getDeposit(t *testing.T, c customer, id uuid.UUID) deposit {
	t.Helper()
	var d deposit
	get(t, c.Token, "/payments/deposits/"+id.String()).decode(t, &d)
	return d
}

func TestDepositLifecycle(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada := register(t, "ada@irate.test")

	key := newKey()
	d, res := startDeposit(t, ada, 750_000, key)
	require.Equal(t, http.StatusCreated, res.Status, string(res.Raw))
	assert.Equal(t, "PENDING", d.Status)

	again, res := startDeposit(t, ada, 750_000, key)
	assert.Equal(t, http.StatusOK, res.Status)
	assert.Equal(t, d.ID, again.ID, "retrying with the same key resumes the same deposit")
	_, res = startDeposit(t, ada, 1, key)
	assert.Equal(t, "IDEMPOTENCY_KEY_REUSED", res.Env.Code)

	require.Equal(t, http.StatusOK, sendWebhook(t, "evt_1", "payment.succeeded", d.Reference, 750_000))
	got := getDeposit(t, ada, d.ID)
	assert.Equal(t, "SUCCEEDED", got.Status)
	assert.NotNil(t, got.TransactionID)
	assert.EqualValues(t, 750_000, balance(t, ada))

	// Redelivery (same event id) and a second, different success event for
	// the same deposit are both no-ops.
	assert.Equal(t, http.StatusOK, sendWebhook(t, "evt_1", "payment.succeeded", d.Reference, 750_000))
	assert.Equal(t, http.StatusOK, sendWebhook(t, "evt_2", "payment.succeeded", d.Reference, 750_000))
	assert.Equal(t, http.StatusOK, sendWebhook(t, "evt_3", "payment.failed", d.Reference, 750_000))
	assert.EqualValues(t, 750_000, balance(t, ada), "credited exactly once")
	assert.Equal(t, "SUCCEEDED", getDeposit(t, ada, d.ID).Status, "failure never overrides success")

	assertLedgerHealthy(t, admin)
}

func TestConcurrentWebhookDeliveriesCreditOnce(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada := register(t, "ada@irate.test")
	d, _ := startDeposit(t, ada, 100_000, newKey())

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			// Half are redeliveries of one event, half are distinct events.
			id := "evt_same"
			if i%2 == 1 {
				id = uuid.NewString()
			}
			assert.Equal(t, http.StatusOK, sendWebhook(t, id, "payment.succeeded", d.Reference, 100_000))
		})
	}
	wg.Wait()
	assert.EqualValues(t, 100_000, balance(t, ada))
	assertLedgerHealthy(t, admin)
}

func TestDepositEdgeCases(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada := register(t, "ada@irate.test")

	t.Run("amount mismatch is flagged, not credited", func(t *testing.T) {
		d, _ := startDeposit(t, ada, 500_000, newKey())
		require.Equal(t, http.StatusOK, sendWebhook(t, uuid.NewString(), "payment.succeeded", d.Reference, 5_000))
		got := getDeposit(t, ada, d.ID)
		assert.Equal(t, "FAILED", got.Status)
		assert.Contains(t, *got.FailureReason, "amount_mismatch")
		assert.EqualValues(t, 0, balance(t, ada))
	})

	t.Run("late success after failure still credits", func(t *testing.T) {
		d, _ := startDeposit(t, ada, 200_000, newKey())
		require.Equal(t, http.StatusOK, sendWebhook(t, uuid.NewString(), "payment.failed", d.Reference, 200_000))
		assert.Equal(t, "FAILED", getDeposit(t, ada, d.ID).Status)
		require.Equal(t, http.StatusOK, sendWebhook(t, uuid.NewString(), "payment.succeeded", d.Reference, 200_000))
		assert.Equal(t, "SUCCEEDED", getDeposit(t, ada, d.ID).Status, "the money really arrived")
		assert.EqualValues(t, 200_000, balance(t, ada))
	})

	t.Run("unknown reference is acknowledged", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, sendWebhook(t, uuid.NewString(), "payment.succeeded", "DEP-NOPE", 1))
	})

	t.Run("forged and unknown-provider webhooks are rejected", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/webhooks/payments/sandbox", bytes.NewReader([]byte(`{"id":"x"}`)))
		r.Header.Set("X-Sandbox-Signature", "00")
		r.Header.Set("X-Sandbox-Timestamp", "1")
		res, err := http.DefaultClient.Do(r)
		require.NoError(t, err)
		res.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, res.StatusCode)

		res, err = http.Post(server.URL+"/api/v1/webhooks/payments/paypal", "application/json", bytes.NewReader([]byte(`{}`)))
		require.NoError(t, err)
		res.Body.Close()
		assert.Equal(t, http.StatusNotFound, res.StatusCode)
	})

	t.Run("deposits are private", func(t *testing.T) {
		d, _ := startDeposit(t, ada, 1_000, newKey())
		bola := register(t, "bola@irate.test")
		assert.Equal(t, http.StatusNotFound, get(t, bola.Token, "/payments/deposits/"+d.ID.String()).Status)
	})

	t.Run("simulate endpoint drives the real webhook path", func(t *testing.T) {
		d, _ := startDeposit(t, ada, 300_000, newKey())
		before := balance(t, ada)
		res := post(t, ada.Token, "/payments/sandbox/deposits/"+d.ID.String()+"/simulate", map[string]string{"outcome": "success"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Raw))
		assert.Equal(t, before+300_000, balance(t, ada))
	})

	assertLedgerHealthy(t, admin)
}

func TestLedgerIsAppendOnlyAtTheDatabase(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada := register(t, "ada@irate.test")
	fund(t, admin, ada, 1_000)

	ctx := context.Background()
	_, err := application.Pool().Exec(ctx, `UPDATE ledger_entries SET amount = amount * 100`)
	assert.ErrorContains(t, err, "append-only")
	_, err = application.Pool().Exec(ctx, `DELETE FROM ledger_entries`)
	assert.ErrorContains(t, err, "append-only")
	_, err = application.Pool().Exec(ctx, `UPDATE wallets SET balance = -1 WHERE kind = 'USER'`)
	assert.ErrorContains(t, err, "wallets_user_balance_non_negative")
}

func TestJanitorCleansExpiredData(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada, bola := register(t, "ada@irate.test"), register(t, "bola@irate.test")
	fund(t, admin, ada, 10_000)
	require.Equal(t, http.StatusOK, transfer(t, ada, bola.Account, 1_000, newKey()).Status)

	ctx := context.Background()
	_, err := application.Pool().Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - interval '25 hours'`)
	require.NoError(t, err)
	require.NoError(t, application.Janitor().RunOnce(ctx))

	var remaining int
	require.NoError(t, application.Pool().QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&remaining))
	assert.Zero(t, remaining)
}
