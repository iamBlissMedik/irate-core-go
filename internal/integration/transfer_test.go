//go:build integration

package integration

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransferHappyPathAndStatements(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada, bola := register(t, "ada@irate.test"), register(t, "bola@irate.test")
	verifyKYC(t, admin, bola) // bola's verified name is shown to ada
	fund(t, admin, ada, 1_000_000)

	res := post(t, ada.Token, "/transactions/transfer",
		map[string]any{"toAccountNumber": bola.Account, "amount": "250000", "note": "  Rent share  "}, "Idempotency-Key", newKey())
	require.Equal(t, http.StatusOK, res.Status, string(res.Raw))
	var line struct {
		Type, Direction, Amount, BalanceAfter, Reference, Description string
		Note                                                          *string
		Counterparty                                                  struct{ AccountNumber, Name string }
	}
	res.decode(t, &line)
	assert.Equal(t, "DEBIT", line.Type)
	assert.Equal(t, "out", line.Direction)
	assert.Equal(t, "250000", line.Amount)
	assert.Equal(t, "750000", line.BalanceAfter)
	assert.Equal(t, "Transfer to "+bola.Account, line.Description)
	assert.Equal(t, "Rent share", *line.Note, "notes are trimmed")
	assert.Equal(t, bola.Account, line.Counterparty.AccountNumber)
	assert.Equal(t, "Test bola", line.Counterparty.Name, "verified KYC name is displayed")

	assert.EqualValues(t, 750_000, balance(t, ada))
	assert.EqualValues(t, 250_000, balance(t, bola))

	var stmt struct {
		Transactions []struct {
			Direction, Amount, BalanceAfter, Reference string
			Counterparty                               *struct{ Name string }
		} `json:"transactions"`
		Pagination struct{ Total int } `json:"pagination"`
	}
	get(t, bola.Token, "/transactions/me").decode(t, &stmt)
	require.Len(t, stmt.Transactions, 1)
	assert.Equal(t, "in", stmt.Transactions[0].Direction)
	assert.Equal(t, line.Reference, stmt.Transactions[0].Reference, "both legs share one reference")
	assert.Equal(t, "Ada", stmt.Transactions[0].Counterparty.Name, "unverified sender falls back to email local part")

	assertLedgerHealthy(t, admin)
}

func TestTransferRejections(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada, bola := register(t, "ada@irate.test"), register(t, "bola@irate.test")
	verifyKYC(t, admin, ada)
	fund(t, admin, ada, 100_000)

	cases := []struct {
		name   string
		to     string
		amount any
		status int
		code   string
	}{
		{"insufficient funds", bola.Account, 100_001, http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS"},
		{"self transfer", ada.Account, 100, http.StatusUnprocessableEntity, "SELF_TRANSFER"},
		{"unknown recipient", "9999999999", 100, http.StatusNotFound, "RECIPIENT_NOT_FOUND"},
		{"system account is not addressable", "0000000001", 100, http.StatusNotFound, "RECIPIENT_NOT_FOUND"},
		{"zero amount", bola.Account, 0, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"negative amount", bola.Account, -100, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"fractional amount", bola.Account, 10.5, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"above single-transaction cap", bola.Account, 1_000_000_001, http.StatusUnprocessableEntity, "AMOUNT_TOO_LARGE"},
		{"malformed account number", "12345", 100, http.StatusBadRequest, "VALIDATION_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := post(t, ada.Token, "/transactions/transfer",
				map[string]any{"toAccountNumber": tc.to, "amount": tc.amount}, "Idempotency-Key", newKey())
			assert.Equal(t, tc.status, res.Status, string(res.Raw))
			assert.Equal(t, tc.code, res.Env.Code)
		})
	}
	assert.EqualValues(t, 100_000, balance(t, ada), "no rejected transfer moved money")

	t.Run("missing idempotency key", func(t *testing.T) {
		res := post(t, ada.Token, "/transactions/transfer", map[string]any{"toAccountNumber": bola.Account, "amount": 1})
		assert.Equal(t, "IDEMPOTENCY_KEY_REQUIRED", res.Env.Code)
	})

	t.Run("failed request can be retried with the same key", func(t *testing.T) {
		key := newKey()
		res := transfer(t, ada, bola.Account, 150_000, key)
		require.Equal(t, "INSUFFICIENT_FUNDS", res.Env.Code)
		fund(t, admin, ada, 100_000)
		res = transfer(t, ada, bola.Account, 150_000, key)
		assert.Equal(t, http.StatusOK, res.Status, "failures are not cached, so the retry executes")
		assert.Empty(t, res.Header.Get("Idempotent-Replayed"))
	})

	t.Run("frozen wallet cannot send but can receive", func(t *testing.T) {
		res := do(t, req{method: http.MethodPatch, path: "/admin/wallets/" + ada.WalletID.String() + "/status", token: admin.Token,
			body: map[string]string{"status": "FROZEN", "reason": "Suspicious activity"}})
		require.Equal(t, http.StatusOK, res.Status, string(res.Raw))

		res = transfer(t, ada, bola.Account, 1, newKey())
		assert.Equal(t, "WALLET_FROZEN", res.Env.Code)

		fund(t, admin, bola, 10_000)
		verifyKYC(t, admin, bola)
		res = transfer(t, bola, ada.Account, 500, newKey())
		assert.Equal(t, http.StatusOK, res.Status, "post-no-debit: incoming money still lands")
	})

	assertLedgerHealthy(t, admin)
}

func TestUnverifiedDailyLimitIsCumulative(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	chidi, ada := register(t, "chidi@irate.test"), register(t, "ada@irate.test")
	fund(t, admin, chidi, 10_000_000)

	require.Equal(t, http.StatusOK, transfer(t, chidi, ada.Account, 3_000_000, newKey()).Status)
	require.Equal(t, http.StatusOK, transfer(t, chidi, ada.Account, 2_000_000, newKey()).Status) // exactly at the limit
	res := transfer(t, chidi, ada.Account, 1, newKey())
	assert.Equal(t, "DAILY_LIMIT_EXCEEDED", res.Env.Code)
	assert.Contains(t, res.Env.Message, "remaining today: NGN 0.00")

	verifyKYC(t, admin, chidi)
	assert.Equal(t, http.StatusOK, transfer(t, chidi, ada.Account, 1_000_000, newKey()).Status, "KYC lifts the limit")
}

func TestIdempotentReplay(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada, bola := register(t, "ada@irate.test"), register(t, "bola@irate.test")
	verifyKYC(t, admin, ada)
	fund(t, admin, ada, 100_000)

	key := newKey()
	first := transfer(t, ada, bola.Account, 40_000, key)
	require.Equal(t, http.StatusOK, first.Status)
	second := transfer(t, ada, bola.Account, 40_000, key)
	require.Equal(t, http.StatusOK, second.Status)
	assert.Equal(t, "true", second.Header.Get("Idempotent-Replayed"))
	assert.JSONEq(t, string(first.Env.Data), string(second.Env.Data), "replay returns the original response")
	assert.EqualValues(t, 60_000, balance(t, ada), "money moved exactly once")

	mismatch := transfer(t, ada, bola.Account, 1, key)
	assert.Equal(t, http.StatusUnprocessableEntity, mismatch.Status)
	assert.Equal(t, "IDEMPOTENCY_KEY_REUSED", mismatch.Env.Code)

	// Keys are scoped per user: bola reusing ada's key is a brand-new request.
	verifyKYC(t, admin, bola)
	other := transfer(t, bola, ada.Account, 40_000, key)
	assert.Equal(t, http.StatusOK, other.Status)
	assert.Empty(t, other.Header.Get("Idempotent-Replayed"), "never replays another user's response")
}

// 50 concurrent transfers race for a balance that covers only 20 of them.
// Pessimistic row locking must let exactly 20 through and never overdraw.
func TestConcurrentTransfersNeverOverdraw(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada, bola := register(t, "ada@irate.test"), register(t, "bola@irate.test")
	verifyKYC(t, admin, ada)
	fund(t, admin, ada, 20_000)

	const attempts, amount = 50, 1_000
	var ok, insufficient, other atomic.Int32
	var wg sync.WaitGroup
	for range attempts {
		wg.Go(func() {
			res := transfer(t, ada, bola.Account, amount, newKey())
			switch {
			case res.Status == http.StatusOK:
				ok.Add(1)
			case res.Env.Code == "INSUFFICIENT_FUNDS":
				insufficient.Add(1)
			default:
				other.Add(1)
				t.Errorf("unexpected response %d: %s", res.Status, res.Raw)
			}
		})
	}
	wg.Wait()

	assert.EqualValues(t, 20, ok.Load())
	assert.EqualValues(t, 30, insufficient.Load())
	assert.Zero(t, other.Load())
	assert.EqualValues(t, 0, balance(t, ada))
	assert.EqualValues(t, 20_000, balance(t, bola))
	assertLedgerHealthy(t, admin)
}

// Transfers in both directions between the same two wallets are the classic
// deadlock setup (A locks X then Y, B locks Y then X). Locking in id order
// must make every one of them succeed.
func TestConcurrentOppositeTransfersDoNotDeadlock(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada, bola := register(t, "ada@irate.test"), register(t, "bola@irate.test")
	verifyKYC(t, admin, ada)
	verifyKYC(t, admin, bola)
	fund(t, admin, ada, 100_000)
	fund(t, admin, bola, 100_000)

	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := range 60 {
		wg.Go(func() {
			from, to := ada, bola
			if i%2 == 1 {
				from, to = bola, ada
			}
			if res := transfer(t, from, to.Account, 1_000, newKey()); res.Status != http.StatusOK {
				failures.Add(1)
				t.Errorf("transfer %d failed: %d %s", i, res.Status, res.Raw)
			}
		})
	}
	wg.Wait()

	assert.Zero(t, failures.Load())
	assert.EqualValues(t, 100_000, balance(t, ada), "30 each way: net zero")
	assert.EqualValues(t, 100_000, balance(t, bola))
	assertLedgerHealthy(t, admin)
}

// The same request fired 20 times at once (e.g. a client retry storm) must
// move money exactly once and give every caller the same answer.
func TestConcurrentSameIdempotencyKey(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada, bola := register(t, "ada@irate.test"), register(t, "bola@irate.test")
	verifyKYC(t, admin, ada)
	fund(t, admin, ada, 100_000)

	key := newKey()
	var wg sync.WaitGroup
	var mu sync.Mutex
	refs := map[string]int{}
	var replays atomic.Int32
	for range 20 {
		wg.Go(func() {
			res := transfer(t, ada, bola.Account, 10_000, key)
			if !assert.Equal(t, http.StatusOK, res.Status, string(res.Raw)) {
				return
			}
			if res.Header.Get("Idempotent-Replayed") == "true" {
				replays.Add(1)
			}
			var line struct{ Reference string }
			res.decode(t, &line)
			mu.Lock()
			refs[line.Reference]++
			mu.Unlock()
		})
	}
	wg.Wait()

	assert.Len(t, refs, 1, "every caller sees the same transaction")
	assert.EqualValues(t, 19, replays.Load())
	assert.EqualValues(t, 90_000, balance(t, ada))
	assert.EqualValues(t, 10_000, balance(t, bola))
	assertLedgerHealthy(t, admin)
}
