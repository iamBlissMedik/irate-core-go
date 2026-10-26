-- name: CreateUser :one
INSERT INTO users (id, email, password_hash, role)
VALUES (@id, @email, @password_hash, @role)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = @id;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = @email;

-- name: ListUsers :many
-- Admin user list. `search` is an ILIKE pattern already escaped by the caller
-- (see httpx.LikePattern); NULL disables the filter.
SELECT u.id, u.email, u.role, u.created_at,
       k.id          AS kyc_id,
       k.status      AS kyc_status,
       k.verified_at AS kyc_verified_at
FROM users u
LEFT JOIN kyc_records k ON k.user_id = u.id
WHERE sqlc.narg('search')::text IS NULL
   OR u.email ILIKE sqlc.narg('search')::text ESCAPE '\'
   OR k.full_name ILIKE sqlc.narg('search')::text ESCAPE '\'
ORDER BY u.created_at DESC, u.id DESC
LIMIT @lim OFFSET @off;

-- name: CountUsers :one
SELECT count(*)
FROM users u
LEFT JOIN kyc_records k ON k.user_id = u.id
WHERE sqlc.narg('search')::text IS NULL
   OR u.email ILIKE sqlc.narg('search')::text ESCAPE '\'
   OR k.full_name ILIKE sqlc.narg('search')::text ESCAPE '\';

-- name: CountCustomers :one
SELECT count(*) FROM users WHERE role = 'USER';

-- name: CountCustomersCreatedBetween :one
SELECT count(*) FROM users
WHERE role = 'USER' AND created_at >= @start_at AND created_at < @end_at;

-- name: UpdateUserPasswordHash :exec
UPDATE users SET password_hash = @password_hash, updated_at = now() WHERE id = @id;
