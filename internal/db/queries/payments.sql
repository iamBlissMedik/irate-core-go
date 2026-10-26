-- name: CreateDeposit :one
-- ON CONFLICT on (user_id, idempotency_key): a retried "start deposit" request
-- returns no row, and the caller loads and resumes the existing deposit.
INSERT INTO deposits (id, user_id, wallet_id, provider, reference, idempotency_key, amount, currency)
VALUES (@id, @user_id, @wallet_id, @provider, @reference, @idempotency_key, @amount, @currency)
ON CONFLICT (user_id, idempotency_key) DO NOTHING
RETURNING *;

-- name: GetDepositByIdempotencyKey :one
SELECT * FROM deposits WHERE user_id = @user_id AND idempotency_key = @idempotency_key;

-- name: SetDepositCheckoutURL :one
UPDATE deposits SET checkout_url = @checkout_url, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: GetDepositForUser :one
SELECT * FROM deposits WHERE id = @id AND user_id = @user_id;

-- name: GetDepositByReference :one
SELECT * FROM deposits WHERE provider = @provider AND reference = @reference;

-- name: LockDepositByReference :one
SELECT * FROM deposits WHERE provider = @provider AND reference = @reference FOR UPDATE;

-- name: MarkDepositSucceeded :one
UPDATE deposits
SET status = 'SUCCEEDED', transaction_id = @transaction_id, failure_reason = NULL,
    completed_at = now(), updated_at = now()
WHERE id = @id AND status <> 'SUCCEEDED'
RETURNING *;

-- name: MarkDepositFailed :one
UPDATE deposits
SET status = 'FAILED', failure_reason = @failure_reason, completed_at = now(), updated_at = now()
WHERE id = @id AND status = 'PENDING'
RETURNING *;

-- name: ListUserDeposits :many
SELECT * FROM deposits WHERE user_id = @user_id
ORDER BY created_at DESC, id DESC
LIMIT @lim OFFSET @off;

-- name: CountUserDeposits :one
SELECT count(*) FROM deposits WHERE user_id = @user_id;

-- name: RecordWebhookEvent :one
-- Returns a row only the first time an event is seen.
INSERT INTO webhook_events (id, provider, event_id, event_type, payload)
VALUES (@id, @provider, @event_id, @event_type, @payload)
ON CONFLICT (provider, event_id) DO NOTHING
RETURNING id;

-- name: MarkWebhookEventProcessed :exec
UPDATE webhook_events SET processed_at = now() WHERE id = @id;
