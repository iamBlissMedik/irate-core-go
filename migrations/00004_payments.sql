-- +goose Up

-- ---------------------------------------------------------------------------
-- Deposits (card/bank funding through an external payment provider)
--
-- Lifecycle: PENDING -> SUCCEEDED | FAILED. A late success webhook may still
-- move FAILED -> SUCCEEDED (the money really arrived). SUCCEEDED is terminal,
-- and the wallet is credited exactly once (guarded by the row lock + status
-- check + transaction_id).
-- ---------------------------------------------------------------------------
CREATE TYPE payment_status AS ENUM ('PENDING', 'SUCCEEDED', 'FAILED');

CREATE TABLE deposits (
    id                 uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid           NOT NULL REFERENCES users (id),
    wallet_id          uuid           NOT NULL REFERENCES wallets (id),
    provider           text           NOT NULL,
    reference          text           NOT NULL,
    idempotency_key    text           NOT NULL,
    amount             bigint         NOT NULL,
    currency           char(3)        NOT NULL,
    status             payment_status NOT NULL DEFAULT 'PENDING',
    checkout_url       text,
    transaction_id     uuid           REFERENCES transactions (id),
    failure_reason     text,
    completed_at       timestamptz,
    created_at         timestamptz    NOT NULL DEFAULT now(),
    updated_at         timestamptz    NOT NULL DEFAULT now(),

    CONSTRAINT deposits_amount_positive CHECK (amount > 0),
    CONSTRAINT deposits_reference_key UNIQUE (provider, reference),
    CONSTRAINT deposits_idempotency_key UNIQUE (user_id, idempotency_key),
    CONSTRAINT deposits_succeeded_has_transaction
        CHECK ((status = 'SUCCEEDED') = (transaction_id IS NOT NULL))
);

CREATE INDEX deposits_user_created_idx ON deposits (user_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- Inbound webhook events (de-duplicated by the provider's event id)
--
-- Providers deliver at-least-once; the unique constraint turns redelivery into
-- a cheap no-op.
-- ---------------------------------------------------------------------------
CREATE TABLE webhook_events (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider     text        NOT NULL,
    event_id     text        NOT NULL,
    event_type   text        NOT NULL,
    payload      jsonb       NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,

    CONSTRAINT webhook_events_provider_event_key UNIQUE (provider, event_id)
);

-- +goose Down
DROP TABLE webhook_events;
DROP TABLE deposits;
DROP TYPE payment_status;
