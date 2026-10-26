# ADR 0003: Idempotency keys in the same Postgres transaction

**Status:** Accepted

## Context

The Express version cached transfer results in Redis under `idem:<key>`:

- it checked the cache, ran the transfer, then wrote the cache, so two
  concurrent requests with the same key **both** transferred;
- keys were global, so user B could receive user A's cached response;
- a crash between commit and cache-write lost the key, so a retry paid twice;
- a Redis flush made every recent key re-executable.

## Decision

Store keys in Postgres (`idempotency_keys`, primary key `(user_id, scope, key)`)
and claim them with `INSERT … ON CONFLICT DO NOTHING` **as the first statement
of the same transaction** that moves the money. Store a hash of the semantic
request fields and the serialized response.

## Consequences

- Movement and key commit atomically, with no window to double-pay.
- A concurrent duplicate blocks on the primary key, then replays. This is
  proven by a 20-way concurrent test.
- A reused key with a different payload gets 422 instead of a wrong replay.
- Failures are not stored (they roll back), so clients may retry after fixing
  the cause.
- A row per mutating request; a janitor deletes rows older than 24 h.
- Deposits, whose provider call can't sit inside a transaction, use
  a unique `(user_id, idempotency_key)` on `deposits` and resume on retry.
