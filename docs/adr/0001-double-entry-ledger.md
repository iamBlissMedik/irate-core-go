# ADR 0001: Double-entry ledger with database-enforced invariants

**Status:** Accepted

## Context

The Express backend stored a `Transaction` row per wallet plus a loosely
coupled `Ledger` row whose counterparty was recovered by regex-matching an
account number in the description. Admin credits created money with no
counter-entry, so there was no way to prove that balances were correct, or
that the platform's books summed to anything.

## Decision

- Every money movement is a **journal**: one `transactions` header plus ≥ 2
  `ledger_entries` lines whose debits equal credits.
- Money entering or leaving the platform posts against **system accounts**
  (`SETTLEMENT`, `ADJUSTMENTS`). Across all wallets, each currency sums to zero.
- `ledger.Post` is the **only** code path that changes `wallets.balance`.
- The invariants are enforced by Postgres, not just by application code:
  a deferred constraint trigger (balanced journal at COMMIT), an append-only
  trigger, `CHECK`s on balance arithmetic and on non-negative customer balances.
- Each entry stores exact `balance_before` and `balance_after`, so statements
  never need recomputation.

## Consequences

- Any balance can be audited from first principles, and `/admin/reconciliation`
  checks all invariants in one call.
- Corrections are new reversing journals; history is never edited.
- The posting engine already supports N-legged journals, so fees (debit user
  amount+fee, credit merchant amount, credit FEES fee) need no schema change.
- Slight write amplification (header + 2 entries + 2 balance updates per
  transfer). That is the standard cost of a correct ledger.
