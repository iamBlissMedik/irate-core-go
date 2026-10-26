-- name: CreateUserWallet :one
-- An account-number collision returns no row (instead of an error that would
-- abort the surrounding transaction), so the caller can retry with a fresh
-- number. A duplicate (user, currency) still raises a unique violation.
INSERT INTO wallets (id, user_id, kind, account_number, currency)
VALUES (@id, @user_id::uuid, 'USER', @account_number, @currency)
ON CONFLICT ON CONSTRAINT wallets_account_number_key DO NOTHING
RETURNING *;

-- name: GetWalletByID :one
SELECT * FROM wallets WHERE id = @id;

-- name: GetUserWallet :one
SELECT * FROM wallets
WHERE user_id = @user_id::uuid AND currency = @currency AND kind = 'USER';

-- name: GetUserWalletByAccountNumber :one
-- Only customer wallets are addressable by account number; system accounts
-- can never be the target of a transfer or a name enquiry.
SELECT * FROM wallets
WHERE account_number = @account_number AND kind = 'USER';

-- name: GetSystemWallet :one
SELECT * FROM wallets
WHERE system_code = @system_code::text AND currency = @currency AND kind = 'SYSTEM';

-- name: ResolveAccount :one
-- Name enquiry. Only a VERIFIED KYC name is trusted for display; otherwise the
-- caller falls back to a name derived from the email's local part.
SELECT w.account_number, u.email, k.full_name AS verified_full_name
FROM wallets w
JOIN users u ON u.id = w.user_id
LEFT JOIN kyc_records k ON k.user_id = u.id AND k.status = 'VERIFIED'
WHERE w.account_number = @account_number AND w.kind = 'USER';

-- name: ListUserWallets :many
SELECT * FROM wallets
WHERE user_id = @user_id::uuid AND kind = 'USER'
ORDER BY created_at DESC, id DESC
LIMIT @lim OFFSET @off;

-- name: CountUserWallets :one
SELECT count(*) FROM wallets WHERE user_id = @user_id::uuid AND kind = 'USER';

-- name: ListWalletIDsByUser :many
SELECT id FROM wallets WHERE user_id = @user_id::uuid AND kind = 'USER';

-- name: ListWalletsByUserIDs :many
SELECT * FROM wallets
WHERE user_id = ANY(@user_ids::uuid[]) AND kind = 'USER'
ORDER BY created_at DESC, id DESC;

-- name: LockWallets :many
-- Pessimistic row locks for a posting. Locking in a deterministic order (by
-- id) means two transfers touching the same pair of wallets in opposite
-- directions always acquire the locks in the same order, so they cannot
-- deadlock.
SELECT * FROM wallets
WHERE id = ANY(@ids::uuid[])
ORDER BY id
FOR UPDATE;

-- name: ApplyWalletDelta :one
UPDATE wallets
SET balance = balance + @delta, updated_at = now()
WHERE id = @id
RETURNING balance;

-- name: SetWalletStatus :one
UPDATE wallets
SET status = @status, updated_at = now()
WHERE id = @id AND kind = 'USER'
RETURNING *;

-- name: SumCustomerBalances :one
SELECT COALESCE(SUM(balance), 0)::bigint AS total, count(*) AS wallets
FROM wallets
WHERE kind = 'USER' AND currency = @currency;

-- name: ListUnreconciledWallets :many
-- Wallets whose stored balance disagrees with the sum of their ledger entries.
-- This must always be empty; a row here means something bypassed the ledger.
SELECT w.id, w.account_number, w.kind, w.currency, w.balance,
       COALESCE(SUM(CASE WHEN e.direction = 'CREDIT' THEN e.amount ELSE -e.amount END), 0)::bigint AS ledger_balance
FROM wallets w
LEFT JOIN ledger_entries e ON e.wallet_id = w.id
GROUP BY w.id
HAVING w.balance <> COALESCE(SUM(CASE WHEN e.direction = 'CREDIT' THEN e.amount ELSE -e.amount END), 0);

-- name: ListNonZeroCurrencyTotals :many
-- Double-entry conservation: customer balances plus system-account balances
-- must net to exactly zero per currency. Any row here means money was created
-- or destroyed outside a balanced posting.
SELECT currency, SUM(balance)::bigint AS net
FROM wallets
GROUP BY currency
HAVING SUM(balance) <> 0;
