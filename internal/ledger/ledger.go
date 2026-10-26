// Package ledger is the single place money moves.
//
// Every movement is a balanced double-entry journal: one transactions row plus
// two or more ledger_entries whose debits and credits are equal. Nothing else
// in the codebase updates wallets.balance.
//
// Concurrency model (pessimistic locking):
//
//  1. Lock every wallet the posting touches with SELECT ... FOR UPDATE,
//     ordered by id. The fixed order means two postings over the same wallets
//     always lock them in the same sequence, so they cannot deadlock.
//  2. While holding the locks, check business rules (balance, frozen status,
//     limits). The rows cannot change underneath us, so these checks cannot
//     race.
//  3. Apply the balance deltas and write entries with exact before/after
//     balances, then commit (which releases the locks).
//
// Optimistic concurrency (version columns + retry) was rejected because hot
// wallets (a merchant receiving many payments) would retry constantly, and
// because balance_before/balance_after must be exact for every entry.
package ledger

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
)

// System account codes (seeded by migration 00002).
const (
	SystemSettlement  = "SETTLEMENT"  // counter-account for money from payment providers
	SystemAdjustments = "ADJUSTMENTS" // counter-account for admin credits
)

var (
	ErrInsufficientFunds = apperr.Unprocessable(apperr.CodeInsufficientFunds, "Insufficient balance.")
	ErrWalletFrozen      = apperr.Unprocessable(apperr.CodeWalletFrozen, "This wallet is frozen and cannot send money. Please contact support.")
	ErrCurrencyMismatch  = apperr.Unprocessable(apperr.CodeCurrencyMismatch, "Both wallets must use the same currency.")
	ErrAmountTooLarge    = apperr.Unprocessable(apperr.CodeAmountTooLarge, "The amount exceeds the maximum allowed for a single transaction.")
)

// Leg is one line of a journal.
type Leg struct {
	WalletID             uuid.UUID
	Direction            db.EntryDirection
	Amount               int64
	Description          string // statement text from this wallet's point of view
	CounterpartyWalletID *uuid.UUID
}

// Posting is a balanced journal to commit.
type Posting struct {
	Kind        db.TransactionKind
	Currency    string
	Description string
	Note        *string
	InitiatedBy *uuid.UUID
	Metadata    map[string]any
	Legs        []Leg
}

// Posted is the committed result.
type Posted struct {
	Transaction db.Transaction
	Entries     []db.LedgerEntry // in the same order as Posting.Legs
}

// Entry returns the entry written for walletID (the first, if several).
func (p Posted) Entry(walletID uuid.UUID) (db.LedgerEntry, bool) {
	for _, e := range p.Entries {
		if e.WalletID == walletID {
			return e, true
		}
	}
	return db.LedgerEntry{}, false
}

// Locked is a set of wallets locked FOR UPDATE in the current transaction.
type Locked map[uuid.UUID]db.Wallet

// Lock takes row locks on the given wallets, in id order. It must run inside
// a transaction (Store.InTx); the locks are held until commit/rollback.
func Lock(ctx context.Context, q *db.Queries, ids ...uuid.UUID) (Locked, error) {
	unique := make([]uuid.UUID, 0, len(ids))
	seen := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	rows, err := q.LockWallets(ctx, unique)
	if err != nil {
		return nil, fmt.Errorf("lock wallets: %w", err)
	}
	if len(rows) != len(unique) {
		return nil, apperr.NotFound(apperr.CodeWalletNotFound, "Wallet not found.")
	}
	locked := make(Locked, len(rows))
	for _, w := range rows {
		locked[w.ID] = w
	}
	return locked, nil
}

// Post validates and writes a posting. Every wallet in p.Legs must already be
// in locked (acquired with Lock in the same transaction).
func Post(ctx context.Context, q *db.Queries, locked Locked, p Posting) (Posted, error) {
	total, err := validate(locked, p)
	if err != nil {
		return Posted{}, err
	}

	meta := p.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		return Posted{}, fmt.Errorf("marshal posting metadata: %w", err)
	}

	txn, err := q.CreateTransaction(ctx, db.CreateTransactionParams{
		ID:          uuid.Must(uuid.NewV7()),
		Reference:   NewReference(p.Kind),
		Kind:        p.Kind,
		Amount:      total,
		Currency:    p.Currency,
		Description: p.Description,
		Note:        p.Note,
		InitiatedBy: p.InitiatedBy,
		Metadata:    rawMeta,
	})
	if err != nil {
		return Posted{}, fmt.Errorf("create transaction: %w", err)
	}

	posted := Posted{Transaction: txn, Entries: make([]db.LedgerEntry, 0, len(p.Legs))}
	for _, leg := range p.Legs {
		delta := leg.Amount
		if leg.Direction == db.EntryDirectionDEBIT {
			delta = -leg.Amount
		}

		w := locked[leg.WalletID]
		before := w.Balance
		after, err := q.ApplyWalletDelta(ctx, db.ApplyWalletDeltaParams{ID: leg.WalletID, Delta: delta})
		switch {
		case database.IsCheckViolation(err, "wallets_user_balance_non_negative"):
			return Posted{}, ErrInsufficientFunds.WithCause(err) // defence in depth
		case database.IsNumericOverflow(err):
			return Posted{}, ErrAmountTooLarge.WithCause(err)
		case err != nil:
			return Posted{}, fmt.Errorf("apply wallet delta: %w", err)
		}
		if after != before+delta {
			// Only possible if the row was modified without holding our lock.
			return Posted{}, fmt.Errorf("ledger invariant violated for wallet %s: expected balance %d, got %d", leg.WalletID, before+delta, after)
		}
		w.Balance = after
		locked[leg.WalletID] = w

		entry, err := q.CreateLedgerEntry(ctx, db.CreateLedgerEntryParams{
			ID:                   uuid.Must(uuid.NewV7()),
			TransactionID:        txn.ID,
			WalletID:             leg.WalletID,
			Direction:            leg.Direction,
			Amount:               leg.Amount,
			BalanceBefore:        before,
			BalanceAfter:         after,
			Description:          leg.Description,
			CounterpartyWalletID: leg.CounterpartyWalletID,
		})
		if err != nil {
			return Posted{}, fmt.Errorf("create ledger entry: %w", err)
		}
		posted.Entries = append(posted.Entries, entry)
	}
	return posted, nil
}

// validate checks the posting is well-formed and allowed, before any write.
func validate(locked Locked, p Posting) (int64, error) {
	if len(p.Legs) < 2 {
		return 0, fmt.Errorf("posting needs at least two legs, got %d", len(p.Legs))
	}

	var debits, credits int64
	net := make(map[uuid.UUID]int64, len(p.Legs))
	for _, leg := range p.Legs {
		if leg.Amount <= 0 {
			return 0, fmt.Errorf("leg amount must be positive, got %d", leg.Amount)
		}
		w, ok := locked[leg.WalletID]
		if !ok {
			return 0, fmt.Errorf("wallet %s is not locked by this transaction", leg.WalletID)
		}
		if w.Currency != p.Currency {
			return 0, ErrCurrencyMismatch
		}
		switch leg.Direction {
		case db.EntryDirectionDEBIT:
			if debits > maxInt64-leg.Amount {
				return 0, ErrAmountTooLarge
			}
			debits += leg.Amount
			net[leg.WalletID] -= leg.Amount
			if w.Kind == db.WalletKindUSER && w.Status == db.WalletStatusFROZEN {
				return 0, ErrWalletFrozen
			}
		case db.EntryDirectionCREDIT:
			if credits > maxInt64-leg.Amount {
				return 0, ErrAmountTooLarge
			}
			credits += leg.Amount
			net[leg.WalletID] += leg.Amount
		default:
			return 0, fmt.Errorf("invalid leg direction %q", leg.Direction)
		}
	}
	if debits != credits {
		return 0, fmt.Errorf("unbalanced posting: debits %d != credits %d", debits, credits)
	}
	for id, delta := range net {
		w := locked[id]
		if w.Kind == db.WalletKindUSER && delta < 0 && w.Balance+delta < 0 {
			return 0, ErrInsufficientFunds
		}
	}
	return debits, nil
}

const maxInt64 = int64(^uint64(0) >> 1)

var refEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewReference returns a human-friendly, unguessable reference such as
// "TRF-20261026-K3J9X2Q7M4N8P1R5" (80 random bits).
func NewReference(kind db.TransactionKind) string {
	prefix := map[db.TransactionKind]string{
		db.TransactionKindTRANSFER:    "TRF",
		db.TransactionKindDEPOSIT:     "DEP",
		db.TransactionKindADMINCREDIT: "ADJ",
	}[kind]
	if prefix == "" {
		prefix = "TXN"
	}
	var b [10]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + time.Now().UTC().Format("20060102") + "-" + strings.ToUpper(refEncoding.EncodeToString(b[:]))
}
