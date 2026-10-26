// Package statement renders ledger entries as bank-statement lines: the
// transaction history feed used by the dashboard and history screens.
package statement

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/money"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
	"github.com/iamBlissMedik/irate-core-go/internal/wallet"
)

// Counterparty is the other customer in a transfer. It is nil for movements
// against system accounts (deposits, admin credits).
type Counterparty struct {
	AccountNumber string `json:"accountNumber"`
	Name          string `json:"name"`
}

// Entry is one statement line, from the viewing wallet's point of view.
// Shape-compatible with transactionEntrySchema in @irate/contracts.
type Entry struct {
	ID            uuid.UUID     `json:"id"`
	TransactionID uuid.UUID     `json:"transactionId"`
	Reference     string        `json:"reference"`
	Kind          string        `json:"kind"`      // TRANSFER | DEPOSIT | ADMIN_CREDIT
	Type          string        `json:"type"`      // CREDIT | DEBIT
	Direction     string        `json:"direction"` // in | out
	Amount        money.Minor   `json:"amount"`
	Currency      string        `json:"currency"`
	Description   string        `json:"description"`
	Note          *string       `json:"note"`
	Counterparty  *Counterparty `json:"counterparty"`
	BalanceAfter  money.Minor   `json:"balanceAfter"`
	CreatedAt     time.Time     `json:"createdAt"`
}

type Page struct {
	Transactions []Entry        `json:"transactions"`
	Pagination   httpx.PageMeta `json:"pagination"`
}

// Build assembles a statement line from freshly posted data, without a
// round-trip to the database.
func Build(entry db.LedgerEntry, txn db.Transaction, counterparty *Counterparty) Entry {
	return Entry{
		ID:            entry.ID,
		TransactionID: txn.ID,
		Reference:     txn.Reference,
		Kind:          string(txn.Kind),
		Type:          string(entry.Direction),
		Direction:     direction(entry.Direction),
		Amount:        money.Minor(entry.Amount),
		Currency:      txn.Currency,
		Description:   entry.Description,
		Note:          txn.Note,
		Counterparty:  counterparty,
		BalanceAfter:  money.Minor(entry.BalanceAfter),
		CreatedAt:     entry.CreatedAt,
	}
}

func fromRow(r db.ListStatementRow) Entry {
	var cp *Counterparty
	if r.CounterpartyAccountNumber != nil && r.CounterpartyEmail != nil {
		cp = &Counterparty{
			AccountNumber: *r.CounterpartyAccountNumber,
			Name:          wallet.DisplayName(*r.CounterpartyEmail, r.CounterpartyVerifiedName),
		}
	}
	return Entry{
		ID:            r.ID,
		TransactionID: r.TransactionID,
		Reference:     r.Reference,
		Kind:          string(r.Kind),
		Type:          string(r.Direction),
		Direction:     direction(r.Direction),
		Amount:        money.Minor(r.Amount),
		Currency:      r.Currency,
		Description:   r.Description,
		Note:          r.Note,
		Counterparty:  cp,
		BalanceAfter:  money.Minor(r.BalanceAfter),
		CreatedAt:     r.CreatedAt,
	}
}

func direction(d db.EntryDirection) string {
	if d == db.EntryDirectionCREDIT {
		return "in"
	}
	return "out"
}

type Service struct {
	store   *database.Store
	wallets *wallet.Service
}

func NewService(store *database.Store, wallets *wallet.Service) *Service {
	return &Service{store: store, wallets: wallets}
}

// ForUser returns statement lines across all of the user's wallets.
func (s *Service) ForUser(ctx context.Context, userID uuid.UUID, p httpx.Page) (Page, error) {
	ids, err := s.store.ListWalletIDsByUser(ctx, userID)
	if err != nil {
		return Page{}, fmt.Errorf("list wallet ids: %w", err)
	}
	return s.forWallets(ctx, ids, p)
}

// ForWallet returns statement lines for one wallet the user owns.
func (s *Service) ForWallet(ctx context.Context, userID, walletID uuid.UUID, p httpx.Page) (Page, error) {
	if err := s.wallets.EnsureOwned(ctx, userID, walletID); err != nil {
		return Page{}, err
	}
	return s.forWallets(ctx, []uuid.UUID{walletID}, p)
}

func (s *Service) forWallets(ctx context.Context, walletIDs []uuid.UUID, p httpx.Page) (Page, error) {
	out := Page{Transactions: []Entry{}, Pagination: httpx.NewPageMeta(0, p)}
	if len(walletIDs) == 0 {
		return out, nil
	}
	rows, err := s.store.ListStatement(ctx, db.ListStatementParams{WalletIds: walletIDs, Lim: p.Lim(), Off: p.Offset()})
	if err != nil {
		return Page{}, fmt.Errorf("list statement: %w", err)
	}
	total, err := s.store.CountStatement(ctx, walletIDs)
	if err != nil {
		return Page{}, fmt.Errorf("count statement: %w", err)
	}
	out.Pagination = httpx.NewPageMeta(total, p)
	for _, r := range rows {
		out.Transactions = append(out.Transactions, fromRow(r))
	}
	return out, nil
}
