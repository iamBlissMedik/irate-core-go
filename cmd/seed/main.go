// Command seed resets the database and loads a reproducible demo dataset.
//
//	go run ./cmd/seed
//
// It REFUSES to run when APP_ENV=production. Everything goes through the real
// domain code (password hashing, wallet opening, ledger postings, KYC review),
// so seeded data satisfies exactly the same invariants as production data.
//
// Result (money in NGN):
//
//	ADMIN  admin@irate.dev  Admin123!     -
//	USER   ada@irate.dev    Password123!  1000000001  48,000.00  KYC verified
//	USER   bola@irate.dev   Password123!  1000000002  14,500.00  KYC verified
//	USER   chidi@irate.dev  Password123!  1000000003       0.00  no KYC (daily limit applies)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/auth"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/ledger"
	"github.com/iamBlissMedik/irate-core-go/internal/money"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/logging"
)

const currency = "NGN"

func naira(n int64) int64 { return n * 100 }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

type seedUser struct {
	email, account, fullName string
	opening                  int64
	verified                 bool
}

func run() error {
	time.Local = time.UTC
	if os.Getenv("APP_ENV") == "production" {
		return errors.New("refusing to run a destructive seed with APP_ENV=production")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx := context.Background()
	log := logging.New(os.Stdout, slog.LevelInfo, "text", "seed")

	pool, err := database.NewPool(ctx, database.PoolConfig{URL: url, MaxConns: 4})
	if err != nil {
		return err
	}
	defer pool.Close()
	store := database.NewStore(pool)

	if err := database.ResetData(ctx, pool); err != nil {
		return err
	}

	hasher := auth.NewHasher(4)
	adminHash, err := hasher.Hash(ctx, env("SEED_ADMIN_PASSWORD", "Admin123!"))
	if err != nil {
		return err
	}
	userHash, err := hasher.Hash(ctx, env("SEED_USER_PASSWORD", "Password123!"))
	if err != nil {
		return err
	}

	users := []seedUser{
		{email: "ada@irate.dev", account: "1000000001", fullName: "Ada Okafor", opening: naira(50_000), verified: true},
		{email: "bola@irate.dev", account: "1000000002", fullName: "Bola Adeyemi", opening: naira(12_500), verified: true},
		{email: "chidi@irate.dev", account: "1000000003", fullName: "Chidi Eze", opening: 0, verified: false},
	}
	wallets := map[string]db.Wallet{}

	err = store.InTx(ctx, func(q *db.Queries) error {
		admin, err := q.CreateUser(ctx, db.CreateUserParams{
			ID: uuid.Must(uuid.NewV7()), Email: auth.NormalizeEmail(env("SEED_ADMIN_EMAIL", "admin@irate.dev")),
			PasswordHash: adminHash, Role: db.UserRoleADMIN,
		})
		if err != nil {
			return fmt.Errorf("create admin: %w", err)
		}
		adjustments, err := q.GetSystemWallet(ctx, db.GetSystemWalletParams{SystemCode: ledger.SystemAdjustments, Currency: currency})
		if err != nil {
			return fmt.Errorf("get adjustments account: %w", err)
		}

		for _, su := range users {
			u, err := q.CreateUser(ctx, db.CreateUserParams{
				ID: uuid.Must(uuid.NewV7()), Email: su.email, PasswordHash: userHash, Role: db.UserRoleUSER,
			})
			if err != nil {
				return fmt.Errorf("create %s: %w", su.email, err)
			}
			w, err := q.CreateUserWallet(ctx, db.CreateUserWalletParams{
				ID: uuid.Must(uuid.NewV7()), UserID: u.ID, AccountNumber: su.account, Currency: currency,
			})
			if err != nil {
				return fmt.Errorf("create wallet for %s: %w", su.email, err)
			}
			wallets[su.email] = w

			if su.verified {
				k, err := q.CreateKYC(ctx, db.CreateKYCParams{
					ID: uuid.Must(uuid.NewV7()), UserID: u.ID, FullName: su.fullName,
					DateOfBirth: time.Date(1994, 5, 17, 0, 0, 0, 0, time.UTC), Address: "12 Admiralty Way, Lekki, Lagos",
					IDType: "NIN", IDNumber: "12345678901",
				})
				if err != nil {
					return fmt.Errorf("create kyc for %s: %w", su.email, err)
				}
				if _, err := q.ReviewPendingKYC(ctx, db.ReviewPendingKYCParams{ID: k.ID, Status: db.KycStatusVERIFIED, ReviewedBy: &admin.ID}); err != nil {
					return fmt.Errorf("verify kyc for %s: %w", su.email, err)
				}
			}

			if su.opening > 0 {
				locked, err := ledger.Lock(ctx, q, adjustments.ID, w.ID)
				if err != nil {
					return err
				}
				if _, err := ledger.Post(ctx, q, locked, ledger.Posting{
					Kind: db.TransactionKindADMINCREDIT, Currency: currency, Description: "Opening balance (seed)",
					InitiatedBy: &admin.ID,
					Legs: []ledger.Leg{
						{WalletID: adjustments.ID, Direction: db.EntryDirectionDEBIT, Amount: su.opening, Description: "Opening balance for " + su.account},
						{WalletID: w.ID, Direction: db.EntryDirectionCREDIT, Amount: su.opening, Description: "Opening balance"},
					},
				}); err != nil {
					return fmt.Errorf("opening balance for %s: %w", su.email, err)
				}
			}
		}

		// One sample transfer so statements and the admin dashboard have data.
		ada, bola := wallets["ada@irate.dev"], wallets["bola@irate.dev"]
		locked, err := ledger.Lock(ctx, q, ada.ID, bola.ID)
		if err != nil {
			return err
		}
		amount := naira(2_000)
		note := "Lunch money"
		_, err = ledger.Post(ctx, q, locked, ledger.Posting{
			Kind: db.TransactionKindTRANSFER, Currency: currency, Note: &note, InitiatedBy: ada.UserID,
			Description: "Transfer from " + ada.AccountNumber + " to " + bola.AccountNumber,
			Legs: []ledger.Leg{
				{WalletID: ada.ID, Direction: db.EntryDirectionDEBIT, Amount: amount, Description: "Transfer to " + bola.AccountNumber, CounterpartyWalletID: &bola.ID},
				{WalletID: bola.ID, Direction: db.EntryDirectionCREDIT, Amount: amount, Description: "Transfer from " + ada.AccountNumber, CounterpartyWalletID: &ada.ID},
			},
		})
		return err
	})
	if err != nil {
		return err
	}

	log.Info("seed complete")
	fmt.Printf("\n%-6s %-18s %-14s %-11s %s\n", "ROLE", "EMAIL", "PASSWORD", "ACCOUNT", "BALANCE")
	fmt.Printf("%-6s %-18s %-14s %-11s %s\n", "ADMIN", env("SEED_ADMIN_EMAIL", "admin@irate.dev"), env("SEED_ADMIN_PASSWORD", "Admin123!"), "-", "-")
	for _, su := range users {
		w, err := store.GetWalletByID(ctx, wallets[su.email].ID)
		if err != nil {
			return err
		}
		fmt.Printf("%-6s %-18s %-14s %-11s %s\n", "USER", su.email, env("SEED_USER_PASSWORD", "Password123!"), su.account, money.Format(money.Minor(w.Balance), currency))
	}
	return nil
}
