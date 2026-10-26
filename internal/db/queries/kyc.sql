-- name: CreateKYC :one
INSERT INTO kyc_records (id, user_id, full_name, date_of_birth, address, id_type, id_number, document_url)
VALUES (@id, @user_id, @full_name, @date_of_birth, @address, @id_type, @id_number, @document_url)
RETURNING *;

-- name: ResubmitRejectedKYC :one
-- Only a REJECTED record may be resubmitted; the status guard makes this safe
-- against a concurrent admin review.
UPDATE kyc_records
SET full_name = @full_name, date_of_birth = @date_of_birth, address = @address,
    id_type = @id_type, id_number = @id_number, document_url = @document_url,
    status = 'PENDING', rejection_reason = NULL, reviewed_by = NULL, reviewed_at = NULL,
    submitted_at = now(), updated_at = now()
WHERE user_id = @user_id AND status = 'REJECTED'
RETURNING *;

-- name: GetKYCByUserID :one
SELECT * FROM kyc_records WHERE user_id = @user_id;

-- name: GetKYCByID :one
SELECT * FROM kyc_records WHERE id = @id;

-- name: ReviewPendingKYC :one
-- Compare-and-set on status: two admins reviewing the same record at once can
-- never both win, and a decided record can never be flipped by accident.
UPDATE kyc_records
SET status = @status,
    rejection_reason = @rejection_reason,
    reviewed_by = @reviewed_by,
    reviewed_at = now(),
    verified_at = CASE WHEN @status::kyc_status = 'VERIFIED' THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = @id AND status = 'PENDING'
RETURNING *;

-- name: ListKYC :many
SELECT k.*, u.email AS user_email, u.role AS user_role
FROM kyc_records k
JOIN users u ON u.id = k.user_id
WHERE sqlc.narg('status')::kyc_status IS NULL OR k.status = sqlc.narg('status')::kyc_status
ORDER BY k.created_at DESC, k.id DESC
LIMIT @lim OFFSET @off;

-- name: CountKYC :one
SELECT count(*) FROM kyc_records
WHERE sqlc.narg('status')::kyc_status IS NULL OR status = sqlc.narg('status')::kyc_status;
