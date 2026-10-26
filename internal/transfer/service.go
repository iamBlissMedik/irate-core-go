// Package transfer implements peer-to-peer transfers by account number.
package transfer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/audit"
	"github.com/iamBlissMedik/irate-core-go/internal/config"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/idempotency"
	"github.com/iamBlissMedik/irate-core-go/internal/ledger"
	"github.com/iamBlissMedik/irate-core-go/internal/money"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/metrics"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/timeutil"
	"github.com/iamBlissMedik/irate-core-go/internal/statement"
	"github.com/iamBlissMedik/irate-core-go/internal/wallet"
)

var (
	ErrNoWallet          = apperr.NotFound(apperr.CodeWalletNotFound, "You don't have a wallet yet.")
	ErrRecipientNotFound = apperr.NotFound(apperr.CodeRecipientNotFound, "Recipient account not found.")
	ErrSelfTransfer      = apperr.Unprocessable(apperr.CodeSelfTransfer, "You cannot transfer money to your own account.")
)

// Request is the transfer payload (amount in minor units).
type Request struct {
	ToAccountNumber string      `json:"toAccountNumber" validate:"required,accountnumber"`
	Amount          money.Minor `json:"amount" validate:"gt=0"`
	Note            string      `json:"note" validate:"max=140,printable"`
}

func (r *Request) Normalize() {
	r.ToAccountNumber = strings.TrimSpace(r.ToAccountNumber)
	r.Note = strings.TrimSpace(r.Note)
}

type Service struct {
	store   *database.Store
	money   config.MoneyConfig
	metrics *metrics.Metrics
	now     func() time.Time
}

func NewService(store *database.Store, moneyCfg config.MoneyConfig, m *metrics.Metrics) *Service {
	return &Service{store: store, money: moneyCfg, metrics: m, now: time.Now}
}

// Transfer moves money from the caller's wallet to the wallet with
// req.ToAccountNumber. It returns the sender's new statement line, and
// replayed=true when the response came from an earlier request with the same
// Idempotency-Key (in which case no money moved this time).
func (s *Service) Transfer(ctx context.Context, userID uuid.UUID, req Request, idemKey string) (out statement.Entry, replayed bool, err error) {
	if err := idempotency.ValidateKey(idemKey); err != nil {
		return out, false, err
	}
	amount := req.Amount.Int64()
	if amount > s.money.MaxTransactionAmount {
		return out, false, apperr.Unprocessable(apperr.CodeAmountTooLarge,
			"The maximum for a single transfer is "+money.Format(money.Minor(s.money.MaxTransactionAmount), s.money.DefaultCurrency)+".")
	}
	key := idempotency.Key{
		UserID:      userID,
		Scope:       idempotency.ScopeTransfer,
		Value:       idemKey,
		RequestHash: idempotency.HashRequest(req.ToAccountNumber, strconv.FormatInt(amount, 10), req.Note),
	}

	err = s.store.InTx(ctx, func(q *db.Queries) error {
		replayed = false // reset: InTx may re-run this closure after a deadlock
		replay, err := idempotency.Claim(ctx, q, key)
		if err != nil {
			return err
		}
		if replay != nil {
			replayed = true
			return json.Unmarshal(replay.Body, &out)
		}

		sender, err := q.GetUserWallet(ctx, db.GetUserWalletParams{UserID: userID, Currency: s.money.DefaultCurrency})
		if database.IsNoRows(err) {
			return ErrNoWallet
		} else if err != nil {
			return fmt.Errorf("get sender wallet: %w", err)
		}
		recipient, err := q.GetUserWalletByAccountNumber(ctx, req.ToAccountNumber)
		if database.IsNoRows(err) {
			return ErrRecipientNotFound
		} else if err != nil {
			return fmt.Errorf("get recipient wallet: %w", err)
		}
		if *recipient.UserID == userID {
			return ErrSelfTransfer
		}

		// From here on both wallets are locked until commit.
		locked, err := ledger.Lock(ctx, q, sender.ID, recipient.ID)
		if err != nil {
			return err
		}
		if err := s.checkDailyLimit(ctx, q, userID, sender.ID, amount); err != nil {
			return err
		}

		var note *string
		if req.Note != "" {
			note = &req.Note
		}
		posted, err := ledger.Post(ctx, q, locked, ledger.Posting{
			Kind:        db.TransactionKindTRANSFER,
			Currency:    sender.Currency,
			Description: fmt.Sprintf("Transfer from %s to %s", sender.AccountNumber, recipient.AccountNumber),
			Note:        note,
			InitiatedBy: &userID,
			Metadata:    map[string]any{"fromAccountNumber": sender.AccountNumber, "toAccountNumber": recipient.AccountNumber},
			Legs: []ledger.Leg{
				{WalletID: sender.ID, Direction: db.EntryDirectionDEBIT, Amount: amount,
					Description: "Transfer to " + recipient.AccountNumber, CounterpartyWalletID: &recipient.ID},
				{WalletID: recipient.ID, Direction: db.EntryDirectionCREDIT, Amount: amount,
					Description: "Transfer from " + sender.AccountNumber, CounterpartyWalletID: &sender.ID},
			},
		})
		if err != nil {
			return err
		}

		resolved, err := q.ResolveAccount(ctx, recipient.AccountNumber)
		if err != nil {
			return fmt.Errorf("resolve recipient name: %w", err)
		}
		entry, _ := posted.Entry(sender.ID)
		out = statement.Build(entry, posted.Transaction, &statement.Counterparty{
			AccountNumber: recipient.AccountNumber,
			Name:          wallet.DisplayName(resolved.Email, resolved.VerifiedFullName),
		})

		if err := audit.Record(ctx, q, audit.Entry{
			ActorID: &userID, Action: audit.ActionTransfer,
			ResourceType: "transaction", ResourceID: posted.Transaction.ID.String(),
			Metadata: map[string]any{
				"reference": posted.Transaction.Reference, "amount": amount, "currency": sender.Currency,
				"toAccountNumber": recipient.AccountNumber,
			},
		}); err != nil {
			return err
		}
		return idempotency.Complete(ctx, q, key, 200, out)
	})
	if err != nil {
		return statement.Entry{}, false, err
	}

	if replayed {
		s.metrics.IdempotentReplays.WithLabelValues(idempotency.ScopeTransfer).Inc()
	} else {
		s.metrics.LedgerPostings.WithLabelValues(string(db.TransactionKindTRANSFER), out.Currency).Inc()
		s.metrics.LedgerPostedMinor.WithLabelValues(string(db.TransactionKindTRANSFER), out.Currency).Add(float64(amount))
	}
	return out, replayed, nil
}

// checkDailyLimit caps outgoing transfers for customers without VERIFIED KYC
// (tiered KYC, as Nigerian regulations require). It runs while the sender's
// wallet is locked, so concurrent transfers cannot each see "room left" and
// jointly exceed the cap.
func (s *Service) checkDailyLimit(ctx context.Context, q *db.Queries, userID, walletID uuid.UUID, amount int64) error {
	limit := s.money.UnverifiedDailyTransferLimit
	if limit <= 0 {
		return nil
	}
	kyc, err := q.GetKYCByUserID(ctx, userID)
	switch {
	case err == nil && kyc.Status == db.KycStatusVERIFIED:
		return nil
	case err != nil && !database.IsNoRows(err):
		return fmt.Errorf("get kyc: %w", err)
	}

	spent, err := q.SumTransferDebitsSince(ctx, db.SumTransferDebitsSinceParams{
		WalletID: walletID, Since: timeutil.StartOfDay(s.now(), s.money.BusinessLocation),
	})
	if err != nil {
		return fmt.Errorf("sum daily transfers: %w", err)
	}
	if spent+amount > limit {
		remaining := max(limit-spent, 0)
		cur := s.money.DefaultCurrency
		return apperr.Unprocessable(apperr.CodeDailyLimitExceeded, fmt.Sprintf(
			"Unverified accounts can send up to %s per day (remaining today: %s). Complete KYC verification to remove this limit.",
			money.Format(money.Minor(limit), cur), money.Format(money.Minor(remaining), cur)))
	}
	return nil
}
