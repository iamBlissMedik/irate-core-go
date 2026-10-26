-- +goose Up

-- ---------------------------------------------------------------------------
-- Wallets
--
-- USER wallets belong to a customer and can never go negative.
-- SYSTEM wallets are the platform's own ledger accounts (e.g. SETTLEMENT for
-- money arriving from a payment provider, ADJUSTMENTS for admin credits). They
-- are the "other side" of every double-entry posting that brings money into or
-- out of the platform, and are allowed to go negative.
-- ---------------------------------------------------------------------------
CREATE TYPE wallet_kind AS ENUM ('USER', 'SYSTEM');

-- FROZEN = "post no debit": the wallet can still receive money, but nothing can
-- leave it until an admin unfreezes it.
CREATE TYPE wallet_status AS ENUM ('ACTIVE', 'FROZEN');

CREATE TABLE wallets (
    id             uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid          REFERENCES users (id) ON DELETE RESTRICT,
    kind           wallet_kind   NOT NULL DEFAULT 'USER',
    system_code    text,
    account_number text          NOT NULL,
    currency       char(3)       NOT NULL DEFAULT 'NGN',
    balance        bigint        NOT NULL DEFAULT 0, -- integer minor units (kobo)
    status         wallet_status NOT NULL DEFAULT 'ACTIVE',
    created_at     timestamptz   NOT NULL DEFAULT now(),
    updated_at     timestamptz   NOT NULL DEFAULT now(),

    CONSTRAINT wallets_account_number_key UNIQUE (account_number),
    CONSTRAINT wallets_account_number_format CHECK (account_number ~ '^[0-9]{10}$'),
    CONSTRAINT wallets_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wallets_owner_matches_kind CHECK (
        (kind = 'USER'   AND user_id IS NOT NULL AND system_code IS NULL) OR
        (kind = 'SYSTEM' AND user_id IS NULL     AND system_code IS NOT NULL)
    ),
    -- Last line of defence against overdrafts. The service checks the balance
    -- under a row lock first; this constraint makes a bug fail loudly instead
    -- of silently creating money.
    CONSTRAINT wallets_user_balance_non_negative CHECK (kind = 'SYSTEM' OR balance >= 0)
);

-- One wallet per user per currency.
CREATE UNIQUE INDEX wallets_user_currency_key ON wallets (user_id, currency) WHERE kind = 'USER';
CREATE UNIQUE INDEX wallets_system_code_currency_key ON wallets (system_code, currency) WHERE kind = 'SYSTEM';

-- System accounts. User account numbers always start with 1-9, so the
-- 000000xxxx range can never collide with a customer.
INSERT INTO wallets (kind, system_code, account_number, currency) VALUES
    ('SYSTEM', 'SETTLEMENT',  '0000000001', 'NGN'),
    ('SYSTEM', 'ADJUSTMENTS', '0000000002', 'NGN');

-- ---------------------------------------------------------------------------
-- Transactions (journal headers) and ledger entries (journal lines)
--
-- Every money movement is ONE transaction with TWO OR MORE ledger entries
-- whose credits and debits net to zero (double-entry bookkeeping).
-- ---------------------------------------------------------------------------
CREATE TYPE transaction_kind AS ENUM ('TRANSFER', 'DEPOSIT', 'ADMIN_CREDIT');

-- Postings are synchronous today, so every row is COMPLETED. PENDING/FAILED/
-- REVERSED exist for asynchronous rails (payouts) on the roadmap.
CREATE TYPE transaction_status AS ENUM ('PENDING', 'COMPLETED', 'FAILED', 'REVERSED');

CREATE TABLE transactions (
    id           uuid               PRIMARY KEY DEFAULT gen_random_uuid(),
    reference    text               NOT NULL,
    kind         transaction_kind   NOT NULL,
    status       transaction_status NOT NULL DEFAULT 'COMPLETED',
    amount       bigint             NOT NULL,
    currency     char(3)            NOT NULL,
    description  text               NOT NULL,
    note         text,
    initiated_by uuid               REFERENCES users (id),
    metadata     jsonb              NOT NULL DEFAULT '{}',
    created_at   timestamptz        NOT NULL DEFAULT now(),

    CONSTRAINT transactions_reference_key UNIQUE (reference),
    CONSTRAINT transactions_amount_positive CHECK (amount > 0)
);

CREATE INDEX transactions_created_at_idx ON transactions (created_at);
CREATE INDEX transactions_kind_created_at_idx ON transactions (kind, created_at);

CREATE TYPE entry_direction AS ENUM ('DEBIT', 'CREDIT');

CREATE TABLE ledger_entries (
    id                     uuid            PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id         uuid            NOT NULL REFERENCES transactions (id),
    wallet_id              uuid            NOT NULL REFERENCES wallets (id),
    direction              entry_direction NOT NULL,
    amount                 bigint          NOT NULL,
    balance_before         bigint          NOT NULL,
    balance_after          bigint          NOT NULL,
    description            text            NOT NULL,
    counterparty_wallet_id uuid            REFERENCES wallets (id),
    created_at             timestamptz     NOT NULL DEFAULT now(),

    CONSTRAINT ledger_entries_amount_positive CHECK (amount > 0),
    CONSTRAINT ledger_entries_balance_math CHECK (
        (direction = 'CREDIT' AND balance_after = balance_before + amount) OR
        (direction = 'DEBIT'  AND balance_after = balance_before - amount)
    )
);

-- Statement queries: newest-first per wallet, with a stable tiebreaker.
CREATE INDEX ledger_entries_wallet_created_idx ON ledger_entries (wallet_id, created_at DESC, id DESC);
CREATE INDEX ledger_entries_transaction_idx ON ledger_entries (transaction_id);
CREATE INDEX ledger_entries_created_idx ON ledger_entries (created_at);

-- The ledger is append-only. Corrections are new, reversing transactions;
-- history is never rewritten. (TRUNCATE does not fire row triggers, which is
-- what the dev seed and integration tests rely on to reset state.)
-- +goose StatementBegin
CREATE FUNCTION forbid_ledger_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only (attempted %)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_ledger_mutation();

-- Double-entry invariant, checked at COMMIT: the entries of a transaction must
-- net to zero. Deferring the check lets a posting insert its legs one by one.
-- +goose StatementBegin
CREATE FUNCTION assert_transaction_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    net bigint;
BEGIN
    SELECT COALESCE(SUM(CASE WHEN direction = 'CREDIT' THEN amount ELSE -amount END), 0)
      INTO net
      FROM ledger_entries
     WHERE transaction_id = NEW.transaction_id;

    IF net <> 0 THEN
        RAISE EXCEPTION 'unbalanced journal for transaction %: net %', NEW.transaction_id, net
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_transaction_balanced();

-- +goose Down
DROP TRIGGER ledger_entries_balanced ON ledger_entries;
DROP FUNCTION assert_transaction_balanced();
DROP TRIGGER ledger_entries_append_only ON ledger_entries;
DROP FUNCTION forbid_ledger_mutation();
DROP TABLE ledger_entries;
DROP TYPE entry_direction;
DROP TABLE transactions;
DROP TYPE transaction_status;
DROP TYPE transaction_kind;
DROP TABLE wallets;
DROP TYPE wallet_status;
DROP TYPE wallet_kind;
