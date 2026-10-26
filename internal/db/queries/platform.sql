-- ---------------------------------------------------------------- sessions

-- name: CreateRefreshToken :exec
INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at, user_agent, ip_address)
VALUES (@id, @user_id, @family_id, @token_hash, @expires_at, @user_agent, @ip_address);

-- name: GetRefreshTokenForUpdate :one
-- Row lock so two concurrent refreshes of the same token serialise: exactly
-- one rotates it, the other sees rotated_at set.
SELECT * FROM refresh_tokens WHERE token_hash = @token_hash FOR UPDATE;

-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens WHERE token_hash = @token_hash;

-- name: MarkRefreshTokenRotated :exec
UPDATE refresh_tokens SET rotated_at = now() WHERE id = @id;

-- name: RevokeRefreshTokenFamily :execrows
UPDATE refresh_tokens SET revoked_at = now()
WHERE family_id = @family_id AND revoked_at IS NULL;

-- name: RevokeUserRefreshTokens :execrows
UPDATE refresh_tokens SET revoked_at = now()
WHERE user_id = @user_id AND revoked_at IS NULL;

-- name: DeleteExpiredRefreshTokens :execrows
DELETE FROM refresh_tokens WHERE expires_at < @before;

-- ------------------------------------------------------------- idempotency

-- name: ClaimIdempotencyKey :one
-- Returns a row only for the first claimant. A concurrent duplicate blocks on
-- the primary key until the first transaction finishes; if it committed, this
-- returns no rows and the caller replays the stored response.
INSERT INTO idempotency_keys (user_id, scope, key, request_hash)
VALUES (@user_id, @scope, @key, @request_hash)
ON CONFLICT DO NOTHING
RETURNING user_id;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys
WHERE user_id = @user_id AND scope = @scope AND key = @key;

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_keys
SET response_status = @response_status, response_body = @response_body
WHERE user_id = @user_id AND scope = @scope AND key = @key;

-- name: DeleteIdempotencyKeysBefore :execrows
DELETE FROM idempotency_keys WHERE created_at < @before;

-- ------------------------------------------------------------------- audit

-- name: CreateAuditLog :exec
INSERT INTO audit_logs (actor_id, action, resource_type, resource_id, metadata, ip_address, request_id)
VALUES (@actor_id, @action, @resource_type, @resource_id, @metadata, @ip_address, @request_id);
