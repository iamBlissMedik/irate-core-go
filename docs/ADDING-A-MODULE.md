# Adding a module

Every feature follows the same vertical slice. This recipe builds a
**savings pots** feature (move money from the wallet into a named pot) as the
example.

## 1. Schema: a migration

```bash
make migrate-new name=savings_pots     # creates migrations/0000X_savings_pots.sql
```

```sql
-- +goose Up
CREATE TABLE savings_pots (
    id         uuid        PRIMARY KEY,
    user_id    uuid        NOT NULL REFERENCES users (id),
    wallet_id  uuid        NOT NULL REFERENCES wallets (id),  -- a dedicated USER wallet holds the pot's money
    name       text        NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX savings_pots_user_idx ON savings_pots (user_id);

-- +goose Down
DROP TABLE savings_pots;
```

Rules: `timestamptz` everywhere, money is `bigint` minor units, name every
constraint you will match on in Go, and write a working `Down`.

> Money inside a pot is still money in a wallet. Give each pot its own wallet
> and move funds with a ledger posting. Never add a second "balance" column
> outside `wallets`.

## 2. Queries

`internal/db/queries/savings.sql`:

```sql
-- name: CreateSavingsPot :one
INSERT INTO savings_pots (id, user_id, wallet_id, name) VALUES (@id, @user_id, @wallet_id, @name)
RETURNING *;

-- name: ListSavingsPots :many
SELECT * FROM savings_pots WHERE user_id = @user_id ORDER BY created_at DESC;
```

Run `make sqlc`. Review the generated types in `internal/db`.

## 3. Service: `internal/savings/service.go`

- Takes `*database.Store` and other services through its constructor.
- Returns `*apperr.Error` for expected outcomes, and wrapped errors otherwise.
- Wraps multi-statement work in `store.InTx`, and moves money only through
  `ledger.Lock` + `ledger.Post`.
- Writes an `audit.Record` in the same transaction for anything money- or security-relevant.
- Requires an `Idempotency-Key` (`idempotency.Claim/Complete`) on any endpoint
  that moves money.

```go
func (s *Service) Fund(ctx context.Context, userID, potID uuid.UUID, req FundRequest, key string) (statement.Entry, bool, error) {
    // ValidateKey → InTx { Claim → load pot (owned by userID) → ledger.Lock(main, pot)
    //   → ledger.Post(debit main, credit pot) → audit → Complete }
}
```

## 4. Handler: `internal/savings/handler.go`

```go
func (h *Handler) Routes(r chi.Router) {
    r.Post("/", httpx.Handle(h.log, h.create))
    r.Get("/", httpx.Handle(h.log, h.list))
    r.Post("/{potId}/fund", httpx.Handle(h.log, h.fund))
}
```

Handlers only: `httpx.Principal`, `httpx.Decode` (request types implement
`Normalize()` and carry `validate:` tags), one service call, then `httpx.OK`.
Use `money.Minor` for amounts in request and response types.

## 5. Wire it: `internal/app/routes.go`

Construct the service and handler, then mount inside the authenticated group:

```go
r.Route("/savings", savingsH.Routes)
```

## 6. Tests

- Unit tests next to the code for pure logic (validation, calculations).
- Integration tests in `internal/integration` for the HTTP flow, including a
  concurrency test if the feature moves money, and an `assertLedgerHealthy`
  call at the end.

## 7. Contract and docs

- Add the endpoints and schemas to `api/openapi.yaml`, then run `npx @redocly/cli lint api/openapi.yaml`.
- Update the API table in the README.
- If the web apps use it, add the schema to `@irate/contracts`.

## Checklist

- [ ] Migration with Up/Down, named constraints, `timestamptz`, `bigint` money
- [ ] Queries in `internal/db/queries`, `make sqlc` run, `make sqlc-check` clean
- [ ] Ownership checks (404 for other users' resources)
- [ ] Money moves only via `ledger`, under `Idempotency-Key`
- [ ] Audit entries for money/security actions
- [ ] Unit + integration tests; `make lint test test-integration` green
- [ ] OpenAPI + README updated
