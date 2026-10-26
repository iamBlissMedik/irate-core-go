# ADR 0006: Redis only for disposable state

**Status:** Accepted

## Context

The Express backend kept refresh tokens, idempotency results and cached wallet
balances in Redis. The balance cache was checked **before** the ownership
check, so any authenticated user could read any wallet's cached balance
(an IDOR), and the cache could disagree with the database.

## Decision

Redis holds only state that is short-lived and safe to lose:

| Data | Store | If Redis is lost |
| --- | --- | --- |
| Rate-limit counters (GCRA) | Redis | Limits reset briefly |
| Login-failure counters | Redis | Lockouts reset |
| Access-token denylist / logout-all cutoff | Redis | Logged-out tokens valid ≤ 15 min (auth fails closed while Redis is *unreachable*) |
| Balances, ledger | Postgres | n/a |
| Sessions (refresh tokens) | Postgres | n/a |
| Idempotency records | Postgres | n/a |

Balances are always read from Postgres (a primary-key lookup), never cached.

## Consequences

- No cache-invalidation bugs on money, and Redis can be flushed or failed over
  without data loss.
- Each dependency failure has an explicit policy: auth fails closed, rate limits
  and lockout fail open (counted in `irate_dependency_fail_open_total`).
