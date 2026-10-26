# ADR 0005: sqlc instead of an ORM

**Status:** Accepted

## Context

The service depends on precise SQL: `FOR UPDATE` with ordering,
`ON CONFLICT … DO NOTHING RETURNING`, compare-and-set `UPDATE … WHERE status = …
RETURNING`, `FILTER` aggregates, and trigram `ILIKE` search. ORMs (GORM, ent)
hide or awkwardly express these, and make N+1 queries easy.

## Decision

Write plain SQL in `internal/db/queries/*.sql` and generate type-safe Go with
**sqlc** (pgx/v5 driver). The schema source is the goose migrations
themselves, so generated types can't drift from the real schema. CI runs
`sqlc diff` to fail on stale generated code.

## Consequences

- Every query is visible, reviewable and EXPLAIN-able; locking intent is explicit.
- Compile-time type safety, and no reflection at runtime.
- Dynamic filters use `sqlc.narg()` (`@x IS NULL OR col = @x`) instead of
  string building, so there are no injection risks.
- Contributors need the `sqlc` binary to regenerate (`make tools`).
