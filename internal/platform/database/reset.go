package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ResetData wipes all business data while keeping the schema and the system
// accounts (whose balances are zeroed). DEVELOPMENT AND TESTS ONLY: callers
// must refuse to run it in production.
//
// TRUNCATE is used for the ledger because it bypasses the append-only row
// triggers, which is exactly the point of a reset and impossible via DELETE.
func ResetData(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		TRUNCATE ledger_entries, transactions, deposits, webhook_events,
		         idempotency_keys, refresh_tokens, audit_logs, kyc_records RESTART IDENTITY;
		DELETE FROM wallets WHERE kind = 'USER';
		UPDATE wallets SET balance = 0, status = 'ACTIVE', updated_at = now() WHERE kind = 'SYSTEM';
		DELETE FROM users;
	`)
	if err != nil {
		return fmt.Errorf("reset data: %w", err)
	}
	return nil
}
