# Architecture

This document explains how the service is put together and, more importantly,
**why**: the invariants it protects and the failure modes it is designed for.

- [Request lifecycle](#request-lifecycle)
- [Layering rules](#layering-rules)
- [The ledger](#the-ledger)
- [Concurrency and locking](#concurrency-and-locking)
- [Idempotency](#idempotency)
- [Authentication and sessions](#authentication-and-sessions)
- [Payments](#payments)
- [Errors](#errors)
- [Failure modes](#failure-modes)
- [Observability](#observability)
- [Scaling](#scaling)

## Request lifecycle

```text
net/http server (read-header/read/write/idle timeouts, 1 MiB header cap)
 └─ RequestID        accept a well-formed X-Request-ID from the BFF, else generate one
 └─ ClientInfo       client IP (X-Forwarded-For trusted only for N proxy hops), User-Agent
 └─ AccessLog        one structured log line + Prometheus metrics per request
 └─ Recoverer        panic → logged stack + JSON 500 (never a dropped connection)
 └─ SecureHeaders    no-store, nosniff, CSP, frame denial, HSTS (prod)
 └─ CORS             exact-origin allowlist, credentials, preflight handling
 └─ Timeout          request context deadline (cancels in-flight SQL)
 └─ /api/v1 router
     ├─ /auth/*            per-IP rate limit on credential endpoints
     ├─ /webhooks/*        no auth; per-provider signature verification
     └─ authenticated group: Authenticator.Required → per-user rate limit
         ├─ /users /wallets /transactions /kyc /payments
         └─ /admin/*       RequireRole(ADMIN)
              handler: httpx.Decode (strict JSON → Normalize → validate)
                └─ service: business rules, Store.InTx(...) → typed apperr errors
                     └─ sqlc queries (internal/db)
              handler: httpx.OK envelope | httpx.Error renders apperr / hides internals
```

## Layering rules

| Layer | May | Must not |
| --- | --- | --- |
| **Handler** (`*/handler.go`) | Parse/validate input, read the principal, call one service method, write the envelope | Contain business rules or SQL |
| **Service** (`*/service.go`) | Enforce rules, open transactions, call `ledger`, `idempotency`, `audit` | Know about HTTP (no `http.Request`, no status writing) |
| **ledger** | Lock wallets, post balanced journals | Know which product triggered the movement |
| **db** (sqlc) | Execute the reviewed SQL in `internal/db/queries` | Be bypassed: there is no ad-hoc SQL in services |

Services return `*apperr.Error` for expected outcomes (with an HTTP status and
a stable `code`) and plain wrapped errors for everything else. The HTTP layer
shows the former verbatim and replaces the latter with a generic 500 carrying
the request id, so internal details never leak.

Dependencies point one way: `app` → handlers → services → (`ledger`,
`idempotency`, `audit`) → `db` / `platform`. The leaf packages `reqctx`,
`apperr` and `money` import nothing internal, which keeps the graph acyclic.

## The ledger

### Model

```text
transactions (journal header)        ledger_entries (journal lines)
─────────────────────────────        ─────────────────────────────────────────────
id, reference (TRF-…/DEP-…/ADJ-…)    transaction_id → transactions
kind: TRANSFER|DEPOSIT|ADMIN_CREDIT  wallet_id      → wallets
amount, currency, note, metadata     direction: DEBIT|CREDIT, amount > 0
initiated_by                         balance_before, balance_after (exact)
                                     counterparty_wallet_id (for statements)
```

`wallets.balance` is a **cached projection** of the ledger, updated in the
same transaction as the entries. Two kinds of wallet exist:

- `USER`: a customer's wallet. `CHECK (balance >= 0)`.
- `SYSTEM`: the platform's own accounts, which may go negative:
  - `SETTLEMENT`: the counter-account for money arriving from payment providers
  - `ADJUSTMENTS`: the counter-account for admin credits

| Movement | Debit | Credit |
| --- | --- | --- |
| P2P transfer | sender | recipient |
| Card deposit | `SETTLEMENT` | customer |
| Admin credit | `ADJUSTMENTS` | customer |

### Invariants and who enforces them

| Invariant | Enforced by |
| --- | --- |
| Every journal balances (Σ debits = Σ credits) | `ledger.validate` before writing, **and** a `DEFERRABLE INITIALLY DEFERRED` constraint trigger at `COMMIT` |
| Entries are never edited or deleted | `BEFORE UPDATE OR DELETE` trigger raising an exception |
| `balance_after = balance_before ± amount` | Row `CHECK` constraint |
| Customer wallets never go negative | Service check under lock, **and** a `CHECK` constraint |
| `wallets.balance` = Σ entries for that wallet | Single write path (`ledger.Post`), verified by `/admin/reconciliation` |
| Σ all balances per currency = 0 (no money created or destroyed) | Double-entry with system accounts, verified by `/admin/reconciliation` |

The application checks come first because they give good error messages; the
database checks exist so that a future bug fails loudly instead of silently
corrupting money.

## Concurrency and locking

The danger is the classic lost update: two transfers read a balance of ₦100
and both debit ₦100. Every posting therefore runs:

```sql
SELECT * FROM wallets WHERE id = ANY($1) ORDER BY id FOR UPDATE;
```

- **`FOR UPDATE`** makes concurrent postings on the same wallet queue up; the
  balance check and the update happen while the row cannot change.
- **`ORDER BY id`** makes every transaction acquire locks in the same global
  order. A→B and B→A transfers can't hold one lock each while waiting
  for the other, so there are no deadlocks.
- **`READ COMMITTED`** isolation is enough because the locks provide the
  serialisation that matters. `Store.InTx` still retries serialization
  failures and deadlocks (SQLSTATE 40001/40P01) up to three times with jitter,
  as defence in depth.
- **`lock_timeout = 5s`**: a request stuck behind an extremely hot wallet fails
  with a retryable 503 instead of holding a pooled connection until timeout.
- **`statement_timeout`** and **`idle_in_transaction_session_timeout`** stop a
  single bad request from holding locks indefinitely.

Other races and how they are closed:

| Race | Resolution |
| --- | --- |
| Two admins review one KYC record | `UPDATE … WHERE id = $1 AND status = 'PENDING' RETURNING`: compare-and-set, one wins, the other gets 409 |
| Two KYC submissions at once | Unique `(user_id)`; the loser gets 409 |
| Concurrent transfers vs. daily limit | The sum of today's debits is computed while the wallet row is locked |
| Freeze during a transfer | The freeze `UPDATE` waits for the row lock, so later transfers see `FROZEN` |
| Two refreshes of one token | `SELECT … FOR UPDATE` on the token row; one rotates, the other sees `rotated_at` |
| Two webhook deliveries for one deposit | Unique `(provider, event_id)` + `FOR UPDATE` on the deposit + status compare-and-set |
| Account-number collision | `INSERT … ON CONFLICT ON CONSTRAINT … DO NOTHING` + retry (no aborted transaction) |
| Several replicas running cleanup | `pg_try_advisory_lock`: one replica works, the others skip |
| Several replicas migrating on boot | goose's Postgres session locker |

## Idempotency

See [ADR 0003](adr/0003-transactional-idempotency.md). In short:

```text
BEGIN
  INSERT INTO idempotency_keys (user_id, scope, key, request_hash) ON CONFLICT DO NOTHING RETURNING
    ├─ row returned → first request: do the work, store the response, COMMIT
    └─ no row       → a committed duplicate exists:
                        hash differs → 422 IDEMPOTENCY_KEY_REUSED
                        hash equal   → replay stored response (+ Idempotent-Replayed: true)
COMMIT
```

A concurrent duplicate blocks on the primary key until the first transaction
ends, so "check then act" races are impossible. Rejected requests roll back
their claim, so a client can fix the problem (top up) and retry with the same
key. Keys expire after `IDEMPOTENCY_TTL` (24 h, the janitor deletes them).

Deposits use the same idea with a unique `(user_id, idempotency_key)` on the
`deposits` table, because the provider call happens outside any transaction.

## Authentication and sessions

See [ADR 0004](adr/0004-refresh-token-rotation.md).

```text
login ──► access JWT (15m, body)  +  refresh token (cookie, 7d sliding, stored as SHA-256)
            │                            │
            │ every request              │ POST /auth/refresh
            ▼                            ▼
   verify sig/alg/iss/aud/exp     lock row → valid? rotate (old.rotated_at = now, new row same family)
   Redis MGET denylist                       already rotated < 20s ago? 401, keep family (racing tabs)
   (jti, user cutoff)                        already rotated earlier?   revoke family (theft) + audit
```

The role is re-read from the database on every refresh, so demotions take
effect within one access-token lifetime.

## Payments

See [PAYMENTS.md](PAYMENTS.md). Provider-agnostic deposit flow: create a
PENDING deposit with our own reference, get a checkout URL from the provider
(outside any DB transaction), and settle on a signed webhook with exactly-once
crediting.

## Errors

| Status | When | Example code |
| --- | --- | --- |
| 400 | Malformed or invalid input | `VALIDATION_ERROR` (with `errors[]`), `BAD_REQUEST`, `IDEMPOTENCY_KEY_REQUIRED` |
| 401 | Missing, invalid, expired or revoked credentials | `ACCESS_TOKEN_EXPIRED`, `INVALID_CREDENTIALS`, `INVALID_SIGNATURE` |
| 403 | Authenticated but not allowed | `FORBIDDEN` |
| 404 | Not found, **or not yours** | `WALLET_NOT_FOUND`, `RECIPIENT_NOT_FOUND` |
| 409 | State conflict | `EMAIL_TAKEN`, `KYC_PENDING`, `KYC_ALREADY_REVIEWED` |
| 413 / 415 | Body too large / not JSON | `PAYLOAD_TOO_LARGE`, `UNSUPPORTED_MEDIA_TYPE` |
| 422 | Valid request that breaks a business rule | `INSUFFICIENT_FUNDS`, `WALLET_FROZEN`, `DAILY_LIMIT_EXCEEDED`, `IDEMPOTENCY_KEY_REUSED` |
| 429 | Rate limited / locked out (`Retry-After` set) | `RATE_LIMITED`, `ACCOUNT_LOCKED` |
| 502 | Payment provider failed | `PAYMENT_PROVIDER_ERROR` |
| 503 | Dependency down, lock timeout, request timeout | `SERVICE_UNAVAILABLE`, `REQUEST_TIMEOUT` |

## Failure modes

| Failure | Behaviour | Why |
| --- | --- | --- |
| Postgres down | Requests fail (500/503), `/readyz` 503 | It is the source of truth; nothing can be served safely |
| Redis down: **authentication** | 503 (fail **closed**) | Can't check the denylist, and a logged-out token must not move money |
| Redis down: **rate limiting** | Requests allowed (fail **open**), metric incremented | Limiter protects capacity; failing closed would turn a cache blip into an outage |
| Redis down: **login lockout** | Logins allowed, metric incremented | argon2 still slows guessing, and IP limits still apply (when Redis returns) |
| Client retries after a timeout | Same `Idempotency-Key` replays the result | The money moved once; the client just missed the response |
| Crash mid-transfer | Postgres rolls the transaction back | Nothing is committed until every leg, audit row and key is written |
| Payment provider down on deposit start | 502; retry with the same key resumes the same deposit | Provider calls happen outside DB transactions |
| Webhook delivered twice / out of order | No-op / success beats failure | Event de-duplication + status compare-and-set |
| Signed success webhook, but provider verify does not confirm (Paystack) | 502, nothing recorded; credited only once verification succeeds on redelivery | A webhook alone (replayed, or forged with a leaked key) can never mint money |
| Provider reports a different amount | Deposit marked FAILED with `amount_mismatch`, audit entry, no credit | Never credit an amount we did not ask for; needs human review |
| SIGTERM | Readiness → 503, in-flight requests drain (`SHUTDOWN_TIMEOUT`), janitor stops, pools close | Zero-downtime rolling deploys |

## Observability

- **Logs:** `log/slog`, JSON in production. One access line per request (method,
  route pattern, status, bytes, duration, IP, request id, user id), and error
  lines with the wrapped cause. Secrets and tokens are never logged.
- **Metrics** (`:9090/metrics`, internal only): request count and latency by route
  pattern; ledger postings and money moved by kind and currency; idempotent
  replays; webhook outcomes; fail-open events; Go runtime and process.
- **Request ids** propagate from the BFF (`X-Request-ID`), appear in every log
  line and error body, and are stored on audit rows.
- **Audit log:** money and security events are written inside the same
  transaction as the action.

## Scaling

- **Stateless API**: run N replicas behind a load balancer. Rate limits,
  lockouts and the denylist are shared through Redis, and cleanup and migration
  coordinate through Postgres advisory locks.
- **Connection budget**: `DB_MAX_CONNS × replicas` must stay below Postgres
  `max_connections`; put PgBouncer (transaction mode) in front at larger scale.
  Note that session advisory locks (janitor, migrations) need session-mode
  connections.
- **Hot wallets**: row locks serialise postings per wallet, which is correct but
  caps a single wallet at roughly one posting per lock-hold time (a few ms). A
  merchant receiving thousands of payments per second would need sharded
  sub-accounts or batched settlement. That's a known, documented limit.
- **Read scaling**: statements and dashboards are read-only and can move to a
  replica (balance checks for transfers must stay on the primary).
- **Growth**: `ledger_entries` is append-only and time-ordered (UUIDv7 ids), which
  suits monthly partitioning and archival.
