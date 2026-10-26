// Package wallet owns the wallet lifecycle, balance reads and account-name
// enquiry. Money movement lives in package ledger.
package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/audit"
	"github.com/iamBlissMedik/irate-core-go/internal/config"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/money"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
)

var (
	ErrNotFound        = apperr.NotFound(apperr.CodeWalletNotFound, "Wallet not found.")
	ErrAccountNotFound = apperr.NotFound(apperr.CodeRecipientNotFound, "Account not found.")
	ErrExists          = apperr.Conflict(apperr.CodeWalletExists, "You already have a wallet in this currency.")
)

const maxAccountNumberAttempts = 5

// Wallet is the public wallet representation.
type Wallet struct {
	ID            uuid.UUID   `json:"id"`
	UserID        uuid.UUID   `json:"userId"`
	AccountNumber string      `json:"accountNumber"`
	Balance       money.Minor `json:"balance"`
	Currency      string      `json:"currency"`
	Status        string      `json:"status"`
	CreatedAt     time.Time   `json:"createdAt"`
}

func FromRow(w db.Wallet) Wallet {
	out := Wallet{
		ID:            w.ID,
		AccountNumber: w.AccountNumber,
		Balance:       money.Minor(w.Balance),
		Currency:      w.Currency,
		Status:        string(w.Status),
		CreatedAt:     w.CreatedAt,
	}
	if w.UserID != nil {
		out.UserID = *w.UserID
	}
	return out
}

type ListResult struct {
	Wallets    []Wallet       `json:"wallets"`
	Pagination httpx.PageMeta `json:"pagination"`
}

type Balance struct {
	WalletID uuid.UUID   `json:"walletId"`
	Balance  money.Minor `json:"balance"`
	Currency string      `json:"currency"`
}

type ResolvedAccount struct {
	AccountNumber string `json:"accountNumber"`
	AccountName   string `json:"accountName"`
}

type Service struct {
	store *database.Store
	money config.MoneyConfig
}

func NewService(store *database.Store, moneyCfg config.MoneyConfig) *Service {
	return &Service{store: store, money: moneyCfg}
}

// Open creates a wallet inside the caller's transaction (used by registration
// and by Create). It retries on account-number collisions; the insert returns
// no row on collision instead of aborting the transaction.
func (s *Service) Open(ctx context.Context, q *db.Queries, userID uuid.UUID, currency string) (db.Wallet, error) {
	if !s.money.SupportsCurrency(currency) {
		return db.Wallet{}, apperr.Unprocessable(apperr.CodeUnsupportedCurrency,
			fmt.Sprintf("Currency %q is not supported. Supported: %s.", currency, strings.Join(s.money.SupportedCurrencies, ", ")))
	}
	for range maxAccountNumberAttempts {
		acct, err := GenerateAccountNumber()
		if err != nil {
			return db.Wallet{}, err
		}
		w, err := q.CreateUserWallet(ctx, db.CreateUserWalletParams{
			ID: uuid.Must(uuid.NewV7()), UserID: userID, AccountNumber: acct, Currency: currency,
		})
		switch {
		case err == nil:
			return w, nil
		case database.IsNoRows(err):
			continue // account number collision: try another
		case database.IsUniqueViolation(err, "wallets_user_currency_key"):
			return db.Wallet{}, ErrExists
		default:
			return db.Wallet{}, fmt.Errorf("create wallet: %w", err)
		}
	}
	return db.Wallet{}, errors.New("could not allocate a unique account number")
}

// Create opens an additional wallet for the caller.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, currency string) (Wallet, error) {
	if currency == "" {
		currency = s.money.DefaultCurrency
	}
	var w db.Wallet
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		var err error
		if w, err = s.Open(ctx, q, userID, currency); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			ActorID: &userID, Action: audit.ActionWalletCreated,
			ResourceType: "wallet", ResourceID: w.ID.String(),
			Metadata: map[string]any{"currency": currency, "accountNumber": w.AccountNumber},
		})
	})
	if err != nil {
		return Wallet{}, err
	}
	return FromRow(w), nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, p httpx.Page) (ListResult, error) {
	rows, err := s.store.ListUserWallets(ctx, db.ListUserWalletsParams{UserID: userID, Lim: p.Lim(), Off: p.Offset()})
	if err != nil {
		return ListResult{}, fmt.Errorf("list wallets: %w", err)
	}
	total, err := s.store.CountUserWallets(ctx, userID)
	if err != nil {
		return ListResult{}, fmt.Errorf("count wallets: %w", err)
	}
	out := ListResult{Wallets: make([]Wallet, 0, len(rows)), Pagination: httpx.NewPageMeta(total, p)}
	for _, w := range rows {
		out.Wallets = append(out.Wallets, FromRow(w))
	}
	return out, nil
}

// Balance reads a wallet's balance straight from Postgres (a primary-key
// lookup). There is deliberately no cache: a cached balance is a second source
// of truth that can disagree with the ledger.
//
// A wallet that does not exist and one owned by somebody else both return
// 404, so the endpoint cannot be used to probe which wallet ids exist.
func (s *Service) Balance(ctx context.Context, userID, walletID uuid.UUID) (Balance, error) {
	w, err := s.ownedWallet(ctx, userID, walletID)
	if err != nil {
		return Balance{}, err
	}
	return Balance{WalletID: w.ID, Balance: money.Minor(w.Balance), Currency: w.Currency}, nil
}

// ownedWallet returns the caller's wallet, or ErrNotFound.
func (s *Service) ownedWallet(ctx context.Context, userID, walletID uuid.UUID) (db.Wallet, error) {
	w, err := s.store.GetWalletByID(ctx, walletID)
	if database.IsNoRows(err) {
		return db.Wallet{}, ErrNotFound
	}
	if err != nil {
		return db.Wallet{}, fmt.Errorf("get wallet: %w", err)
	}
	if w.Kind != db.WalletKindUSER || w.UserID == nil || *w.UserID != userID {
		return db.Wallet{}, ErrNotFound
	}
	return w, nil
}

// EnsureOwned verifies the caller owns walletID.
func (s *Service) EnsureOwned(ctx context.Context, userID, walletID uuid.UUID) error {
	_, err := s.ownedWallet(ctx, userID, walletID)
	return err
}

// Resolve is the "name enquiry" before a transfer: account number -> display
// name. It never returns the email, balance or any internal id.
func (s *Service) Resolve(ctx context.Context, accountNumber string) (ResolvedAccount, error) {
	if !isAccountNumber(accountNumber) {
		return ResolvedAccount{}, apperr.Field("accountNumber", "must be exactly 10 digits")
	}
	row, err := s.store.ResolveAccount(ctx, accountNumber)
	if database.IsNoRows(err) {
		return ResolvedAccount{}, ErrAccountNotFound
	}
	if err != nil {
		return ResolvedAccount{}, fmt.Errorf("resolve account: %w", err)
	}
	return ResolvedAccount{AccountNumber: row.AccountNumber, AccountName: DisplayName(row.Email, row.VerifiedFullName)}, nil
}

func isAccountNumber(s string) bool {
	if len(s) != 10 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
