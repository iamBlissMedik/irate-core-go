# ADR 0002: Pessimistic row locking with ordered acquisition

**Status:** Accepted

## Context

The Express transfer read the sender's balance, then ran a conditional
`UPDATE … WHERE balance >= amount`. That prevented overdrafts, but the ledger
row's `balanceBefore` came from the stale read, so concurrent transfers wrote
wrong running balances. The recipient was updated without any lock-order
discipline.

## Options

1. **Conditional UPDATE only**: no overdraft, but no consistent before/after
   snapshot, and multi-rule checks (limits, frozen status) can't be atomic.
2. **Optimistic locking** (version column + retry): good under low contention,
   but hot wallets retry constantly and every caller needs retry logic.
3. **SERIALIZABLE isolation**: correct, but aborts are frequent under load and
   harder to reason about.
4. **Pessimistic `SELECT … FOR UPDATE`** on every wallet in the posting.

## Decision

Option 4, with locks always acquired in `ORDER BY id`. Run under READ COMMITTED,
with `lock_timeout` (5s), `statement_timeout`, and automatic retry of
40001/40P01 as defence in depth.

## Consequences

- All checks (balance, frozen, daily limit) run on rows that cannot change, so
  they are race-free by construction.
- A fixed global lock order rules out deadlocks between postings. This is
  proven by an integration test with 60 opposite-direction transfers.
- Throughput per wallet is bounded by lock-hold time (milliseconds). That's fine
  for consumer wallets; very hot merchant accounts would need sharding
  (documented in ARCHITECTURE.md → Scaling).
