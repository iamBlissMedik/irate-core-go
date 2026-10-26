package ledger

import (
	"math"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamBlissMedik/irate-core-go/internal/db"
)

func userWallet(balance int64, status db.WalletStatus) db.Wallet {
	uid := uuid.New()
	return db.Wallet{ID: uuid.New(), UserID: &uid, Kind: db.WalletKindUSER, Currency: "NGN", Balance: balance, Status: status}
}

func systemWallet(balance int64) db.Wallet {
	code := "ADJUSTMENTS"
	return db.Wallet{ID: uuid.New(), Kind: db.WalletKindSYSTEM, SystemCode: &code, Currency: "NGN", Balance: balance, Status: db.WalletStatusACTIVE}
}

func transfer(from, to db.Wallet, amount int64) Posting {
	return Posting{
		Kind: db.TransactionKindTRANSFER, Currency: "NGN",
		Legs: []Leg{
			{WalletID: from.ID, Direction: db.EntryDirectionDEBIT, Amount: amount},
			{WalletID: to.ID, Direction: db.EntryDirectionCREDIT, Amount: amount},
		},
	}
}

func lockedOf(ws ...db.Wallet) Locked {
	l := Locked{}
	for _, w := range ws {
		l[w.ID] = w
	}
	return l
}

func TestValidate(t *testing.T) {
	rich := userWallet(10_000, db.WalletStatusACTIVE)
	poor := userWallet(500, db.WalletStatusACTIVE)
	frozen := userWallet(10_000, db.WalletStatusFROZEN)
	sys := systemWallet(0)
	usd := userWallet(10_000, db.WalletStatusACTIVE)
	usd.Currency = "USD"
	stranger := userWallet(10_000, db.WalletStatusACTIVE)

	t.Run("valid transfer returns total", func(t *testing.T) {
		total, err := validate(lockedOf(rich, poor), transfer(rich, poor, 10_000))
		require.NoError(t, err)
		assert.Equal(t, int64(10_000), total, "spending the exact balance is allowed")
	})
	t.Run("insufficient funds", func(t *testing.T) {
		_, err := validate(lockedOf(rich, poor), transfer(poor, rich, 501))
		assert.ErrorIs(t, err, ErrInsufficientFunds)
	})
	t.Run("system accounts may go negative", func(t *testing.T) {
		_, err := validate(lockedOf(sys, poor), transfer(sys, poor, 1_000_000))
		assert.NoError(t, err)
	})
	t.Run("frozen wallet cannot send", func(t *testing.T) {
		_, err := validate(lockedOf(frozen, poor), transfer(frozen, poor, 1))
		assert.ErrorIs(t, err, ErrWalletFrozen)
	})
	t.Run("frozen wallet can receive (post-no-debit)", func(t *testing.T) {
		_, err := validate(lockedOf(rich, frozen), transfer(rich, frozen, 1))
		assert.NoError(t, err)
	})
	t.Run("currency mismatch", func(t *testing.T) {
		_, err := validate(lockedOf(rich, usd), transfer(rich, usd, 1))
		assert.ErrorIs(t, err, ErrCurrencyMismatch)
	})
	t.Run("unbalanced", func(t *testing.T) {
		p := transfer(rich, poor, 100)
		p.Legs[1].Amount = 99
		_, err := validate(lockedOf(rich, poor), p)
		assert.ErrorContains(t, err, "unbalanced")
	})
	t.Run("single leg", func(t *testing.T) {
		p := transfer(rich, poor, 100)
		p.Legs = p.Legs[:1]
		_, err := validate(lockedOf(rich, poor), p)
		assert.Error(t, err)
	})
	t.Run("non-positive amount", func(t *testing.T) {
		_, err := validate(lockedOf(rich, poor), transfer(rich, poor, 0))
		assert.Error(t, err)
		_, err = validate(lockedOf(rich, poor), transfer(rich, poor, -5))
		assert.Error(t, err)
	})
	t.Run("wallet not locked by this transaction", func(t *testing.T) {
		_, err := validate(lockedOf(rich), transfer(rich, stranger, 1))
		assert.ErrorContains(t, err, "not locked")
	})
	t.Run("overflow", func(t *testing.T) {
		s1, s2 := systemWallet(0), systemWallet(0)
		p := Posting{Kind: db.TransactionKindADMINCREDIT, Currency: "NGN", Legs: []Leg{
			{WalletID: s1.ID, Direction: db.EntryDirectionDEBIT, Amount: math.MaxInt64},
			{WalletID: s1.ID, Direction: db.EntryDirectionDEBIT, Amount: 1},
			{WalletID: s2.ID, Direction: db.EntryDirectionCREDIT, Amount: 1},
		}}
		_, err := validate(lockedOf(s1, s2), p)
		assert.ErrorIs(t, err, ErrAmountTooLarge)
	})
	t.Run("net per wallet: a wallet debited twice is checked on the total", func(t *testing.T) {
		p := Posting{Kind: db.TransactionKindTRANSFER, Currency: "NGN", Legs: []Leg{
			{WalletID: poor.ID, Direction: db.EntryDirectionDEBIT, Amount: 300},
			{WalletID: poor.ID, Direction: db.EntryDirectionDEBIT, Amount: 300}, // e.g. amount + fee
			{WalletID: rich.ID, Direction: db.EntryDirectionCREDIT, Amount: 600},
		}}
		_, err := validate(lockedOf(rich, poor), p)
		assert.ErrorIs(t, err, ErrInsufficientFunds)
	})
}

func TestNewReference(t *testing.T) {
	re := regexp.MustCompile(`^(TRF|DEP|ADJ)-\d{8}-[A-Z2-7]{16}$`)
	seen := map[string]bool{}
	for _, k := range []db.TransactionKind{db.TransactionKindTRANSFER, db.TransactionKindDEPOSIT, db.TransactionKindADMINCREDIT} {
		for range 200 {
			ref := NewReference(k)
			assert.Regexp(t, re, ref)
			assert.False(t, seen[ref], "references must be unique")
			seen[ref] = true
		}
	}
}

func TestPostedEntry(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	p := Posted{Entries: []db.LedgerEntry{{WalletID: a, Amount: 1}, {WalletID: b, Amount: 2}}}
	e, ok := p.Entry(b)
	require.True(t, ok)
	assert.Equal(t, int64(2), e.Amount)
	_, ok = p.Entry(uuid.New())
	assert.False(t, ok)
}
