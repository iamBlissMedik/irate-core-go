// Package user serves user profiles, the customer home-screen overview and
// the admin user directory.
package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/kyc"
	"github.com/iamBlissMedik/irate-core-go/internal/money"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
	"github.com/iamBlissMedik/irate-core-go/internal/statement"
	"github.com/iamBlissMedik/irate-core-go/internal/wallet"
)

var ErrNotFound = apperr.NotFound(apperr.CodeNotFound, "User not found.")

const recentTransactions = 5

// Profile is a user with their KYC record and wallets (userSchema).
type Profile struct {
	ID        uuid.UUID       `json:"id"`
	Email     string          `json:"email"`
	Role      string          `json:"role"`
	KYCStatus *string         `json:"kycStatus,omitempty"`
	KYC       *kyc.Record     `json:"kyc"`
	Wallets   []wallet.Wallet `json:"wallets"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// Overview is the customer home screen (userOverviewSchema).
type Overview struct {
	WalletID           uuid.UUID         `json:"walletId"`
	AccountNumber      string            `json:"accountNumber"`
	Balance            money.Minor       `json:"balance"`
	Currency           string            `json:"currency"`
	WalletStatus       string            `json:"walletStatus"`
	KYCStatus          *string           `json:"kycStatus"`
	Stats              OverviewStats     `json:"stats"`
	RecentTransactions []statement.Entry `json:"recentTransactions"`
}

type OverviewStats struct {
	TotalIn          money.Minor `json:"totalIn"`
	TotalOut         money.Minor `json:"totalOut"`
	TransactionCount int64       `json:"transactionCount"`
}

// AdminRow is one row of the admin user directory (adminUserRowSchema).
type AdminRow struct {
	ID        uuid.UUID       `json:"id"`
	Email     string          `json:"email"`
	Role      string          `json:"role"`
	CreatedAt time.Time       `json:"createdAt"`
	KYC       *AdminKYC       `json:"kyc"`
	Wallets   []wallet.Wallet `json:"wallets"`
}

type AdminKYC struct {
	ID         uuid.UUID  `json:"id"`
	Status     string     `json:"status"`
	VerifiedAt *time.Time `json:"verifiedAt"`
}

type AdminList struct {
	Users      []AdminRow     `json:"users"`
	Pagination httpx.PageMeta `json:"pagination"`
}

type Service struct {
	store      *database.Store
	kyc        *kyc.Service
	statements *statement.Service
	currency   string
}

func NewService(store *database.Store, kycSvc *kyc.Service, statements *statement.Service, defaultCurrency string) *Service {
	return &Service{store: store, kyc: kycSvc, statements: statements, currency: defaultCurrency}
}

// ProfileFor returns targetID's profile. Customers may only read their own;
// admins may read anyone's. The permission check happens BEFORE the lookup, so
// a 403 never reveals whether the id exists (no IDOR, no existence oracle).
func (s *Service) ProfileFor(ctx context.Context, viewer reqctx.Principal, targetID uuid.UUID) (Profile, error) {
	if targetID != viewer.UserID && !viewer.IsAdmin() {
		return Profile{}, apperr.Forbidden("You can only view your own profile.")
	}
	return s.Profile(ctx, targetID)
}

func (s *Service) Profile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	u, err := s.store.GetUserByID(ctx, userID)
	if database.IsNoRows(err) {
		return Profile{}, ErrNotFound
	} else if err != nil {
		return Profile{}, fmt.Errorf("get user: %w", err)
	}
	walletRows, err := s.store.ListWalletsByUserIDs(ctx, []uuid.UUID{userID})
	if err != nil {
		return Profile{}, fmt.Errorf("list wallets: %w", err)
	}

	p := Profile{
		ID: u.ID, Email: u.Email, Role: string(u.Role), CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
		Wallets: make([]wallet.Wallet, 0, len(walletRows)),
	}
	for _, w := range walletRows {
		p.Wallets = append(p.Wallets, wallet.FromRow(w))
	}
	rec, err := s.kyc.Mine(ctx, userID)
	switch {
	case err == nil:
		p.KYC, p.KYCStatus = &rec, &rec.Status
	case !errors.Is(err, kyc.ErrNotFound):
		return Profile{}, err
	}
	return p, nil
}

// Overview builds the customer home screen for the default-currency wallet.
func (s *Service) Overview(ctx context.Context, userID uuid.UUID) (Overview, error) {
	w, err := s.store.GetUserWallet(ctx, db.GetUserWalletParams{UserID: userID, Currency: s.currency})
	if database.IsNoRows(err) {
		return Overview{}, wallet.ErrNotFound
	} else if err != nil {
		return Overview{}, fmt.Errorf("get wallet: %w", err)
	}
	totals, err := s.store.WalletFlowTotals(ctx, w.ID)
	if err != nil {
		return Overview{}, fmt.Errorf("wallet totals: %w", err)
	}
	recent, err := s.statements.ForWallet(ctx, userID, w.ID, httpx.Page{Page: 1, Limit: recentTransactions})
	if err != nil {
		return Overview{}, err
	}

	ov := Overview{
		WalletID: w.ID, AccountNumber: w.AccountNumber, Balance: money.Minor(w.Balance),
		Currency: w.Currency, WalletStatus: string(w.Status),
		Stats: OverviewStats{
			TotalIn: money.Minor(totals.TotalIn), TotalOut: money.Minor(totals.TotalOut), TransactionCount: totals.EntryCount,
		},
		RecentTransactions: recent.Transactions,
	}
	if rec, err := s.kyc.Mine(ctx, userID); err == nil {
		ov.KYCStatus = &rec.Status
	} else if !errors.Is(err, kyc.ErrNotFound) {
		return Overview{}, err
	}
	return ov, nil
}

// List is the admin user directory with search over email and KYC name.
func (s *Service) List(ctx context.Context, search string, p httpx.Page) (AdminList, error) {
	pattern := httpx.LikePattern(search)
	rows, err := s.store.ListUsers(ctx, db.ListUsersParams{Search: pattern, Lim: p.Lim(), Off: p.Offset()})
	if err != nil {
		return AdminList{}, fmt.Errorf("list users: %w", err)
	}
	total, err := s.store.CountUsers(ctx, pattern)
	if err != nil {
		return AdminList{}, fmt.Errorf("count users: %w", err)
	}

	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	walletsByUser := map[uuid.UUID][]wallet.Wallet{}
	if len(ids) > 0 {
		ws, err := s.store.ListWalletsByUserIDs(ctx, ids) // one query, not N+1
		if err != nil {
			return AdminList{}, fmt.Errorf("list wallets: %w", err)
		}
		for _, w := range ws {
			walletsByUser[*w.UserID] = append(walletsByUser[*w.UserID], wallet.FromRow(w))
		}
	}

	out := AdminList{Users: make([]AdminRow, 0, len(rows)), Pagination: httpx.NewPageMeta(total, p)}
	for _, r := range rows {
		row := AdminRow{ID: r.ID, Email: r.Email, Role: string(r.Role), CreatedAt: r.CreatedAt, Wallets: walletsByUser[r.ID]}
		if row.Wallets == nil {
			row.Wallets = []wallet.Wallet{}
		}
		if r.KycID != nil && r.KycStatus.Valid {
			row.KYC = &AdminKYC{ID: *r.KycID, Status: string(r.KycStatus.KycStatus), VerifiedAt: r.KycVerifiedAt}
		}
		out.Users = append(out.Users, row)
	}
	return out, nil
}
