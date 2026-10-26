// Package admin implements back-office operations: dashboard statistics,
// manual wallet credits, freezing wallets and ledger reconciliation.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

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
	"github.com/iamBlissMedik/irate-core-go/internal/wallet"
)

// StatCard is a dashboard tile (adminStatCardSchema). Value is a count
// (number) or money (minor-unit string).
type StatCard struct {
	Title     string       `json:"title"`
	Value     any          `json:"value"`
	Trend     *float64     `json:"trend,omitempty"`
	TrendType string       `json:"trendType,omitempty"`
	Currency  string       `json:"currency,omitempty"`
	Wallets   *int64       `json:"wallets,omitempty"`
	Credit    *money.Minor `json:"credit,omitempty"`
	Debit     *money.Minor `json:"debit,omitempty"`
}

type Overview struct {
	UsersStats        StatCard `json:"usersStats"`
	WalletsBalance    StatCard `json:"walletsBalance"`
	TransactionStats  StatCard `json:"transactionStats"`
	TotalCashflow     StatCard `json:"totalCashflow"`
	TransactionVolume StatCard `json:"transactionVolume"`
}

type CreditRequest struct {
	Amount money.Minor `json:"amount" validate:"gt=0"`
	Reason string      `json:"reason" validate:"required,max=200,printable"`
}

func (r *CreditRequest) Normalize() { r.Reason = strings.TrimSpace(r.Reason) }

type CreditResult struct {
	WalletID      uuid.UUID   `json:"walletId"`
	AccountNumber string      `json:"accountNumber"`
	Amount        money.Minor `json:"amount"`
	Balance       money.Minor `json:"balance"`
	Currency      string      `json:"currency"`
	TransactionID uuid.UUID   `json:"transactionId"`
	Reference     string      `json:"reference"`
	CreatedAt     time.Time   `json:"createdAt"`
}

type WalletStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=ACTIVE FROZEN"`
	Reason string `json:"reason" validate:"required,max=200,printable"`
}

func (r *WalletStatusRequest) Normalize() {
	r.Status = strings.ToUpper(strings.TrimSpace(r.Status))
	r.Reason = strings.TrimSpace(r.Reason)
}

type Reconciliation struct {
	Healthy                bool               `json:"healthy"`
	UnbalancedTransactions int64              `json:"unbalancedTransactions"`
	WalletMismatches       []WalletMismatch   `json:"walletMismatches"`
	NonZeroCurrencies      []CurrencyNetTotal `json:"nonZeroCurrencies"`
	CheckedAt              time.Time          `json:"checkedAt"`
}

type WalletMismatch struct {
	WalletID      uuid.UUID   `json:"walletId"`
	AccountNumber string      `json:"accountNumber"`
	Kind          string      `json:"kind"`
	Currency      string      `json:"currency"`
	Balance       money.Minor `json:"balance"`
	LedgerBalance money.Minor `json:"ledgerBalance"`
}

type CurrencyNetTotal struct {
	Currency string      `json:"currency"`
	Net      money.Minor `json:"net"`
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

// ------------------------------------------------------------- dashboard

// Overview computes all dashboard tiles concurrently.
func (s *Service) Overview(ctx context.Context) (Overview, error) {
	var ov Overview
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { ov.UsersStats, err = s.UsersStats(ctx); return })
	g.Go(func() (err error) { ov.WalletsBalance, err = s.WalletsBalance(ctx); return })
	g.Go(func() (err error) { ov.TransactionStats, err = s.transactionStats(ctx); return })
	g.Go(func() (err error) { ov.TotalCashflow, err = s.totalCashflow(ctx); return })
	g.Go(func() (err error) { ov.TransactionVolume, err = s.transactionVolume(ctx); return })
	return ov, g.Wait()
}

// days returns [start of today, start of tomorrow) and the same for yesterday,
// in the business timezone.
func (s *Service) days() (todayStart, tomorrowStart, yesterdayStart time.Time) {
	todayStart = timeutil.StartOfDay(s.now(), s.money.BusinessLocation)
	return todayStart, todayStart.AddDate(0, 0, 1), todayStart.AddDate(0, 0, -1)
}

// UsersStats: total customers (admins excluded) and today's sign-ups vs
// yesterday's.
func (s *Service) UsersStats(ctx context.Context) (StatCard, error) {
	today, tomorrow, yesterday := s.days()
	total, err := s.store.CountCustomers(ctx)
	if err != nil {
		return StatCard{}, fmt.Errorf("count customers: %w", err)
	}
	cur, err := s.store.CountCustomersCreatedBetween(ctx, db.CountCustomersCreatedBetweenParams{StartAt: today, EndAt: tomorrow})
	if err != nil {
		return StatCard{}, fmt.Errorf("count new customers: %w", err)
	}
	prev, err := s.store.CountCustomersCreatedBetween(ctx, db.CountCustomersCreatedBetweenParams{StartAt: yesterday, EndAt: today})
	if err != nil {
		return StatCard{}, fmt.Errorf("count new customers: %w", err)
	}
	trend, trendType := PercentChange(cur, prev)
	return StatCard{Title: "Total Users", Value: total, Trend: &trend, TrendType: trendType}, nil
}

// WalletsBalance: money held in customer wallets (system accounts excluded)
// and today's inflows vs yesterday's.
func (s *Service) WalletsBalance(ctx context.Context) (StatCard, error) {
	cur := s.money.DefaultCurrency
	today, tomorrow, yesterday := s.days()
	sum, err := s.store.SumCustomerBalances(ctx, cur)
	if err != nil {
		return StatCard{}, fmt.Errorf("sum balances: %w", err)
	}
	in, err := s.store.SumCustomerCreditsBetween(ctx, db.SumCustomerCreditsBetweenParams{Currency: cur, StartAt: today, EndAt: tomorrow})
	if err != nil {
		return StatCard{}, fmt.Errorf("sum credits: %w", err)
	}
	prevIn, err := s.store.SumCustomerCreditsBetween(ctx, db.SumCustomerCreditsBetweenParams{Currency: cur, StartAt: yesterday, EndAt: today})
	if err != nil {
		return StatCard{}, fmt.Errorf("sum credits: %w", err)
	}
	trend, trendType := PercentChange(in, prevIn)
	wallets := sum.Wallets
	return StatCard{
		Title: "Total Wallet Balance", Value: money.Minor(sum.Total), Currency: cur,
		Wallets: &wallets, Trend: &trend, TrendType: trendType,
	}, nil
}

// transactionStats: all-time journal count, last 24h vs the 24h before.
func (s *Service) transactionStats(ctx context.Context) (StatCard, error) {
	now := s.now()
	total, err := s.store.CountTransactions(ctx)
	if err != nil {
		return StatCard{}, fmt.Errorf("count transactions: %w", err)
	}
	cur, err := s.store.CountTransactionsBetween(ctx, db.CountTransactionsBetweenParams{StartAt: now.Add(-24 * time.Hour), EndAt: now})
	if err != nil {
		return StatCard{}, fmt.Errorf("count transactions: %w", err)
	}
	prev, err := s.store.CountTransactionsBetween(ctx, db.CountTransactionsBetweenParams{StartAt: now.Add(-48 * time.Hour), EndAt: now.Add(-24 * time.Hour)})
	if err != nil {
		return StatCard{}, fmt.Errorf("count transactions: %w", err)
	}
	trend, trendType := PercentChange(cur, prev)
	return StatCard{Title: "Total Transactions", Value: total, Trend: &trend, TrendType: trendType}, nil
}

// totalCashflow: money that entered customer wallets minus money that left.
// P2P transfers cancel out, so this is the platform's net external inflow.
func (s *Service) totalCashflow(ctx context.Context) (StatCard, error) {
	cur := s.money.DefaultCurrency
	t, err := s.store.CustomerFlowTotals(ctx, cur)
	if err != nil {
		return StatCard{}, fmt.Errorf("cashflow totals: %w", err)
	}
	credit, debit := money.Minor(t.Credits), money.Minor(t.Debits)
	return StatCard{Title: "Total Cashflow", Value: credit - debit, Credit: &credit, Debit: &debit, Currency: cur}, nil
}

// transactionVolume: money moved in the last 24h vs the 24h before. Each
// transfer is counted once (the original counted both legs, doubling it).
func (s *Service) transactionVolume(ctx context.Context) (StatCard, error) {
	cur, now := s.money.DefaultCurrency, s.now()
	vol, err := s.store.SumTransactionVolumeBetween(ctx, db.SumTransactionVolumeBetweenParams{Currency: cur, StartAt: now.Add(-24 * time.Hour), EndAt: now})
	if err != nil {
		return StatCard{}, fmt.Errorf("sum volume: %w", err)
	}
	prev, err := s.store.SumTransactionVolumeBetween(ctx, db.SumTransactionVolumeBetweenParams{Currency: cur, StartAt: now.Add(-48 * time.Hour), EndAt: now.Add(-24 * time.Hour)})
	if err != nil {
		return StatCard{}, fmt.Errorf("sum volume: %w", err)
	}
	trend, trendType := PercentChange(vol, prev)
	return StatCard{Title: "Transaction Volume", Value: money.Minor(vol), Currency: cur, Trend: &trend, TrendType: trendType}, nil
}

// PercentChange returns the % change from prev to cur (one decimal) and
// "up" | "down" | "neutral". From zero, any growth is reported as +100%.
func PercentChange(cur, prev int64) (float64, string) {
	trendType := "neutral"
	switch {
	case cur > prev:
		trendType = "up"
	case cur < prev:
		trendType = "down"
	}
	if prev == 0 {
		if cur > 0 {
			return 100, trendType
		}
		return 0, trendType
	}
	pct := float64(cur-prev) / float64(prev) * 100
	return math.Round(pct*10) / 10, trendType
}

// ---------------------------------------------------------------- credit

// Credit adds money to a customer wallet (manual top-up / goodwill /
// correction). The other side of the journal is the ADJUSTMENTS system
// account, so the ledger stays balanced and every naira is traceable.
func (s *Service) Credit(ctx context.Context, adminID, walletID uuid.UUID, req CreditRequest, idemKey string) (out CreditResult, replayed bool, err error) {
	if err := idempotency.ValidateKey(idemKey); err != nil {
		return out, false, err
	}
	amount := req.Amount.Int64()
	if amount > s.money.MaxTransactionAmount {
		return out, false, ledger.ErrAmountTooLarge
	}
	key := idempotency.Key{
		UserID: adminID, Scope: idempotency.ScopeAdminCredit, Value: idemKey,
		RequestHash: idempotency.HashRequest(walletID.String(), strconv.FormatInt(amount, 10), req.Reason),
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

		target, err := q.GetWalletByID(ctx, walletID)
		if database.IsNoRows(err) || (err == nil && target.Kind != db.WalletKindUSER) {
			return wallet.ErrNotFound
		} else if err != nil {
			return fmt.Errorf("get wallet: %w", err)
		}
		if *target.UserID == adminID {
			return apperr.Forbidden("Admins cannot credit their own wallet.")
		}
		sys, err := q.GetSystemWallet(ctx, db.GetSystemWalletParams{SystemCode: ledger.SystemAdjustments, Currency: target.Currency})
		if err != nil {
			return fmt.Errorf("get %s account for %s: %w", ledger.SystemAdjustments, target.Currency, err)
		}

		locked, err := ledger.Lock(ctx, q, target.ID, sys.ID)
		if err != nil {
			return err
		}
		posted, err := ledger.Post(ctx, q, locked, ledger.Posting{
			Kind:        db.TransactionKindADMINCREDIT,
			Currency:    target.Currency,
			Description: "Admin credit: " + req.Reason,
			InitiatedBy: &adminID,
			Metadata:    map[string]any{"reason": req.Reason, "accountNumber": target.AccountNumber},
			Legs: []ledger.Leg{
				{WalletID: sys.ID, Direction: db.EntryDirectionDEBIT, Amount: amount, Description: "Admin credit to " + target.AccountNumber},
				{WalletID: target.ID, Direction: db.EntryDirectionCREDIT, Amount: amount, Description: req.Reason},
			},
		})
		if err != nil {
			return err
		}
		entry, _ := posted.Entry(target.ID)
		out = CreditResult{
			WalletID: target.ID, AccountNumber: target.AccountNumber, Amount: money.Minor(amount),
			Balance: money.Minor(entry.BalanceAfter), Currency: target.Currency,
			TransactionID: posted.Transaction.ID, Reference: posted.Transaction.Reference, CreatedAt: posted.Transaction.CreatedAt,
		}
		if err := audit.Record(ctx, q, audit.Entry{
			ActorID: &adminID, Action: audit.ActionAdminCredit, ResourceType: "wallet", ResourceID: target.ID.String(),
			Metadata: map[string]any{"amount": amount, "currency": target.Currency, "reason": req.Reason, "reference": posted.Transaction.Reference},
		}); err != nil {
			return err
		}
		return idempotency.Complete(ctx, q, key, 200, out)
	})
	if err != nil {
		return CreditResult{}, false, err
	}
	if replayed {
		s.metrics.IdempotentReplays.WithLabelValues(idempotency.ScopeAdminCredit).Inc()
	} else {
		s.metrics.LedgerPostings.WithLabelValues(string(db.TransactionKindADMINCREDIT), out.Currency).Inc()
		s.metrics.LedgerPostedMinor.WithLabelValues(string(db.TransactionKindADMINCREDIT), out.Currency).Add(float64(amount))
	}
	return out, replayed, nil
}

// SetWalletStatus freezes or unfreezes a customer wallet. The UPDATE takes the
// row lock, so it waits for any in-flight transfer on that wallet to finish
// and every later transfer sees the new status.
func (s *Service) SetWalletStatus(ctx context.Context, adminID, walletID uuid.UUID, req WalletStatusRequest) (wallet.Wallet, error) {
	var w db.Wallet
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		var err error
		w, err = q.SetWalletStatus(ctx, db.SetWalletStatusParams{ID: walletID, Status: db.WalletStatus(req.Status)})
		if database.IsNoRows(err) {
			return wallet.ErrNotFound
		} else if err != nil {
			return fmt.Errorf("set wallet status: %w", err)
		}
		return audit.Record(ctx, q, audit.Entry{
			ActorID: &adminID, Action: audit.ActionWalletStatus, ResourceType: "wallet", ResourceID: walletID.String(),
			Metadata: map[string]any{"status": req.Status, "reason": req.Reason},
		})
	})
	if err != nil {
		return wallet.Wallet{}, err
	}
	return wallet.FromRow(w), nil
}

// --------------------------------------------------------- reconciliation

// Reconcile checks the ledger's invariants. All three must always hold; any
// finding means something bypassed the ledger and needs investigation.
func (s *Service) Reconcile(ctx context.Context) (Reconciliation, error) {
	unbalanced, err := s.store.CountUnbalancedTransactions(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("count unbalanced: %w", err)
	}
	mismatches, err := s.store.ListUnreconciledWallets(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("list unreconciled: %w", err)
	}
	nonZero, err := s.store.ListNonZeroCurrencyTotals(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("currency totals: %w", err)
	}

	out := Reconciliation{
		UnbalancedTransactions: unbalanced,
		WalletMismatches:       make([]WalletMismatch, 0, len(mismatches)),
		NonZeroCurrencies:      make([]CurrencyNetTotal, 0, len(nonZero)),
		CheckedAt:              s.now().UTC(),
	}
	for _, m := range mismatches {
		out.WalletMismatches = append(out.WalletMismatches, WalletMismatch{
			WalletID: m.ID, AccountNumber: m.AccountNumber, Kind: string(m.Kind), Currency: m.Currency,
			Balance: money.Minor(m.Balance), LedgerBalance: money.Minor(m.LedgerBalance),
		})
	}
	for _, c := range nonZero {
		out.NonZeroCurrencies = append(out.NonZeroCurrencies, CurrencyNetTotal{Currency: c.Currency, Net: money.Minor(c.Net)})
	}
	out.Healthy = unbalanced == 0 && len(out.WalletMismatches) == 0 && len(out.NonZeroCurrencies) == 0
	return out, nil
}
