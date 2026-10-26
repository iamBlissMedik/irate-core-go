-- +goose Up

-- ---------------------------------------------------------------------------
-- Refresh-token sessions
--
-- Refresh tokens are opaque random strings; only their SHA-256 hash is stored,
-- so a database leak does not leak usable tokens. Every refresh ROTATES the
-- token (the old row gets rotated_at, a new row joins the same family). If an
-- already-rotated token is presented again outside a short grace window, the
-- whole family is revoked: a stolen token gets the thief at most one refresh.
-- ---------------------------------------------------------------------------
CREATE TABLE refresh_tokens (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id  uuid        NOT NULL,
    token_hash bytea       NOT NULL,
    expires_at timestamptz NOT NULL,
    rotated_at timestamptz,
    revoked_at timestamptz,
    user_agent text,
    ip_address text,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT refresh_tokens_token_hash_key UNIQUE (token_hash)
);

CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);
CREATE INDEX refresh_tokens_expires_idx ON refresh_tokens (expires_at);

-- ---------------------------------------------------------------------------
-- Idempotency keys
--
-- Written in the SAME database transaction as the money movement it protects,
-- so "the transfer happened" and "the key is recorded" commit atomically. A
-- concurrent duplicate blocks on the primary key until the first request
-- commits, then replays the stored response. Keys are scoped per user, so one
-- customer can never replay another customer's response.
-- ---------------------------------------------------------------------------
CREATE TABLE idempotency_keys (
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    scope           text        NOT NULL,
    key             text        NOT NULL,
    request_hash    text        NOT NULL,
    response_status integer     NOT NULL DEFAULT 0,
    response_body   jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, scope, key)
);

CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

-- ---------------------------------------------------------------------------
-- Audit log
--
-- Money-moving actions write their audit row inside the same transaction as
-- the movement, so an audit record exists if and only if the action committed.
-- ---------------------------------------------------------------------------
CREATE TABLE audit_logs (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_id      uuid,
    action        text        NOT NULL,
    resource_type text,
    resource_id   text,
    metadata      jsonb       NOT NULL DEFAULT '{}',
    ip_address    text,
    request_id    text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_actor_created_idx ON audit_logs (actor_id, created_at DESC);
CREATE INDEX audit_logs_created_idx ON audit_logs (created_at DESC);

-- +goose Down
DROP TABLE audit_logs;
DROP TABLE idempotency_keys;
DROP TABLE refresh_tokens;
