-- +goose Up

-- pg_trgm powers the admin user search (ILIKE '%term%') with a GIN index
-- instead of a sequential scan. It is a "trusted" extension since PG13, so the
-- database owner can create it without superuser.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TYPE user_role AS ENUM ('USER', 'ADMIN');

CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    role          user_role   NOT NULL DEFAULT 'USER',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    -- Emails are normalised (trimmed + lower-cased) by the application. The
    -- check guarantees "Ada@x.com" and "ada@x.com" can never both exist.
    CONSTRAINT users_email_lowercase CHECK (email = lower(email)),
    CONSTRAINT users_email_key UNIQUE (email)
);

CREATE INDEX users_created_at_idx ON users (created_at DESC, id DESC);
CREATE INDEX users_email_trgm_idx ON users USING gin (email gin_trgm_ops);

CREATE TYPE kyc_status AS ENUM ('PENDING', 'VERIFIED', 'REJECTED');

CREATE TABLE kyc_records (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    full_name        text        NOT NULL,
    date_of_birth    date        NOT NULL,
    address          text        NOT NULL,
    id_type          text        NOT NULL,
    id_number        text        NOT NULL,
    document_url     text,
    status           kyc_status  NOT NULL DEFAULT 'PENDING',
    rejection_reason text,
    reviewed_by      uuid        REFERENCES users (id),
    reviewed_at      timestamptz,
    verified_at      timestamptz,
    submitted_at     timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    -- One KYC record per user; resubmission after rejection updates the row.
    CONSTRAINT kyc_records_user_id_key UNIQUE (user_id),
    CONSTRAINT kyc_records_id_type_check
        CHECK (id_type IN ('NIN', 'BVN', 'PASSPORT', 'DRIVERS_LICENSE', 'VOTERS_CARD')),
    CONSTRAINT kyc_records_verified_at_check
        CHECK ((status = 'VERIFIED') = (verified_at IS NOT NULL))
);

CREATE INDEX kyc_records_status_created_idx ON kyc_records (status, created_at DESC);
CREATE INDEX kyc_records_full_name_trgm_idx ON kyc_records USING gin (full_name gin_trgm_ops);

-- +goose Down
DROP TABLE kyc_records;
DROP TYPE kyc_status;
DROP TABLE users;
DROP TYPE user_role;
