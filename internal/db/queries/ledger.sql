-- name: CreateTransaction :one
INSERT INTO transactions (id, reference, kind, amount, currency, description, note, initiated_by, metadata)
VALUES (@id, @reference, @kind, @amount, @currency, @description, @note, @initiated_by, @metadata)
RETURNING *;

-- name: CreateLedgerEntry :one
INSERT INTO ledger_entries (id, transaction_id, wallet_id, direction, amount,
                            balance_before, balance_after, description, counterparty_wallet_id)
VALUES (@id, @transaction_id, @wallet_id, @direction, @amount,
        @balance_before, @balance_after, @description, @counterparty_wallet_id)
RETURNING *;

-- name: ListStatement :many
-- Bank-statement lines for a set of wallets, newest first. The counterparty is
-- only resolved for customer wallets (system accounts are never exposed).
SELECT e.id, e.transaction_id, t.reference, t.kind, t.note, e.direction, e.amount,
       t.currency, e.description, e.balance_after, e.created_at,
       cw.account_number AS counterparty_account_number,
       cu.email          AS counterparty_email,
       ck.full_name      AS counterparty_verified_name
FROM ledger_entries e
JOIN transactions t ON t.id = e.transaction_id
LEFT JOIN wallets cw ON cw.id = e.counterparty_wallet_id AND cw.kind = 'USER'
LEFT JOIN users cu ON cu.id = cw.user_id
LEFT JOIN kyc_records ck ON ck.user_id = cu.id AND ck.status = 'VERIFIED'
WHERE e.wallet_id = ANY(@wallet_ids::uuid[])
ORDER BY e.created_at DESC, e.id DESC
LIMIT @lim OFFSET @off;

-- name: CountStatement :one
SELECT count(*) FROM ledger_entries WHERE wallet_id = ANY(@wallet_ids::uuid[]);

-- name: WalletFlowTotals :one
-- Lifetime money in / out of one wallet.
SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'CREDIT'), 0)::bigint AS total_in,
       COALESCE(SUM(amount) FILTER (WHERE direction = 'DEBIT'), 0)::bigint  AS total_out,
       count(*) AS entry_count
FROM ledger_entries
WHERE wallet_id = @wallet_id;

-- name: SumTransferDebitsSince :one
-- Outgoing P2P transfers from a wallet since a point in time (daily limits).
-- Called while the wallet row is locked, so the result cannot go stale.
SELECT COALESCE(SUM(e.amount), 0)::bigint
FROM ledger_entries e
JOIN transactions t ON t.id = e.transaction_id
WHERE e.wallet_id = @wallet_id
  AND e.direction = 'DEBIT'
  AND t.kind = 'TRANSFER'
  AND e.created_at >= @since;

-- name: CountTransactions :one
SELECT count(*) FROM transactions;

-- name: CountTransactionsBetween :one
SELECT count(*) FROM transactions
WHERE created_at >= @start_at AND created_at < @end_at;

-- name: SumTransactionVolumeBetween :one
SELECT COALESCE(SUM(amount), 0)::bigint FROM transactions
WHERE currency = @currency AND created_at >= @start_at AND created_at < @end_at;

-- name: CustomerFlowTotals :one
-- Money in / out of customer wallets. P2P transfers cancel out, so the net is
-- money that entered the platform from outside (deposits, admin credits).
SELECT COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'CREDIT'), 0)::bigint AS credits,
       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'DEBIT'), 0)::bigint  AS debits
FROM ledger_entries e
JOIN wallets w ON w.id = e.wallet_id
WHERE w.kind = 'USER' AND w.currency = @currency;

-- name: SumCustomerCreditsBetween :one
SELECT COALESCE(SUM(e.amount), 0)::bigint
FROM ledger_entries e
JOIN wallets w ON w.id = e.wallet_id
WHERE w.kind = 'USER' AND w.currency = @currency AND e.direction = 'CREDIT'
  AND e.created_at >= @start_at AND e.created_at < @end_at;

-- name: CountUnbalancedTransactions :one
-- Must always be zero (also enforced by a deferred constraint trigger).
SELECT count(*) FROM (
    SELECT transaction_id
    FROM ledger_entries
    GROUP BY transaction_id
    HAVING SUM(CASE WHEN direction = 'CREDIT' THEN amount ELSE -amount END) <> 0
) unbalanced;
