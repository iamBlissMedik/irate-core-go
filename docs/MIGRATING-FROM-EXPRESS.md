# Migrating from the Express backend

`irate-core-go` replaces the Express backend, [irate-core](https://github.com/iamBlissMedik/irate-core) (Express 5 + Prisma + Redis). This
document records what changed for API clients, and the defects in the original
that the rewrite fixes. Every defect below was confirmed by reading the
original source.

## Compatibility with the web apps

The HTTP contract in `@irate/contracts` is preserved: same paths, same
`{ success, message, data }` envelope, money as minor-unit strings, the same
list shapes (`{ users | kycs | wallets | transactions, pagination }`), the same
admin stat cards, and the refresh token in a cookie named `refreshToken`
(rotated tokens arrive via `Set-Cookie`, which the BFF already reads).

Differences a client may notice:

| Area | Express | Go |
| --- | --- | --- |
| Login response | `accessToken` at the top level | `data.accessToken` (the BFF already reads both) plus `tokenType`, `expiresIn` |
| Register response | `{ id, email, accountNumber }` | `{ user: {id,email,role,createdAt}, accountNumber }`, the shape in `registerResultSchema` |
| Error body | `{ success, message, stack? }` | `{ success, message, code, errors?, requestId }`; stack traces are never sent |
| Status codes | Most failures were 400 | 409 conflicts, 422 business rules, 401 bad credentials, 429 lockout |
| Wrong email at login | 404 "Email not found" | 401 "Invalid email or password" (same as wrong password) |
| Statement line | `id` = per-wallet transaction row | `id` = ledger entry; adds `transactionId`, `reference`, `kind`, `currency`, `note` |
| `GET /users/:id` | Nested paginated wallets → transactions (`walletPage`, `txPage`…) | Profile with `kyc`, `kycStatus`, `wallets`; use the statement endpoints for history |
| IDs | cuid | UUIDv7 (time-ordered) |
| Password policy | ≥ 6 chars | 8–128, upper + lower + digit (matches the frontend schema) |
| KYC input | Any strings | `idType` enum, 11-digit NIN/BVN, age ≥ 18, `documentUrl` must be a URL |
| Transfers | No KYC rule | Unverified users have a daily cap (₦50,000, configurable) |
| Idempotency-Key | Transfers only | Required on transfers, admin credits and deposits (the web client already sends one on every mutation) |
| Unknown JSON fields | Ignored | Rejected (catches typos like `ammount`) |

New endpoints: `POST /auth/logout-all`, `/payments/*`, `/webhooks/payments/{provider}`,
`PATCH /admin/wallets/{id}/status`, `GET /admin/reconciliation`, `/healthz`, `/readyz`.

## Defects fixed

### Money

1. **Concurrent duplicate transfers.** Idempotency was check-cache → transfer →
   write-cache in Redis (`transaction.service.ts`), so two simultaneous requests
   with the same key both moved money. A crash between commit and cache write
   also lost the key. *Fix:* key claimed inside the same DB transaction ([ADR 0003](adr/0003-transactional-idempotency.md)).
2. **Cross-user idempotency replay.** Cache keys were `idem:<key>` with no user,
   so user B reusing a key got user A's transfer result. *Fix:* keys scoped by user.
3. **Wrong running balances.** The transfer read `from.balance` before the
   conditional debit, then wrote `balanceBefore`/`balanceAfter` from that stale
   read, so concurrent transfers recorded incorrect statement balances.
   Admin credit (`creditWallet`) read the balance without a lock too. *Fix:*
   `FOR UPDATE` locks and exact per-entry balances ([ADR 0002](adr/0002-pessimistic-locking.md)).
4. **Money created from nothing.** Admin credits and the seed's opening balances
   had no counter-entry, so the books could never be reconciled. *Fix:*
   double-entry against system accounts ([ADR 0001](adr/0001-double-entry-ledger.md)).
5. **Mutable ledger.** Nothing stopped updates or deletes of ledger rows.
   *Fix:* append-only trigger, a balance-math `CHECK`, and a balanced-journal trigger.
6. **Precision loss.** `z.coerce.number()` turned `"9007199254740993"` into
   `9007199254740992` before `BigInt()`. *Fix:* amounts parsed straight into `int64`
   from the JSON text; floats and exponents rejected.
7. **Fragile counterparties.** Statement counterparties were recovered by
   regex-matching a 10-digit number in the description text. *Fix:* a
   `counterparty_wallet_id` foreign key.
8. **Dashboard double counting.** Volume and transaction counts summed both legs
   of every transfer; "users today" counted admins while "total users" excluded them.

### Security

9. **Wallet balance IDOR.** `getBalance` returned a Redis-cached balance
   **before** checking ownership, so any user could read any wallet's balance
   once it was cached. *Fix:* no balance cache; ownership checked first, with
   404 for wallets owned by someone else ([ADR 0006](adr/0006-redis-for-disposable-state.md)).
10. **Account enumeration.** Login answered 404 "Email not found" vs 401
    "Invalid password". *Fix:* one generic 401, plus a dummy hash for unknown
    emails so response timing matches.
11. **Name-enquiry impersonation.** The display name came from the KYC
    `fullName` regardless of status, so anyone could show as "Dangote" while
    their KYC was pending. *Fix:* only VERIFIED names are shown.
12. **Refresh tokens.** One per user (a second device logged out the first),
    never rotated (a stolen token lasted 7 days), the role was copied from the old
    token forever, and logout needed a valid access token. Raw access tokens
    were also used as Redis keys. *Fix:* [ADR 0004](adr/0004-refresh-token-rotation.md).
13. **Rate limiting behind the BFF.** 30 requests/minute **per IP**, and every
    BFF request comes from the same server IP, so all users shared one budget.
    *Fix:* per-user limits for authenticated routes, per-IP for anonymous ones.
14. **CORS rejection crashed to 500.** A disallowed origin raised an error into
    the global handler. *Fix:* no CORS headers, so the browser blocks it.
15. **Unbounded pagination.** `limit=1000000` was honoured. *Fix:* clamped to 100.
16. **LIKE wildcard injection** in admin search (`%` matched everything). *Fix:* escaped.

### Workflow correctness

17. **KYC review had no state guard.** A VERIFIED record could be re-reviewed
    and flipped to REJECTED, and two admins could both "win". *Fix:* compare-and-set
    on `PENDING`; decisions are final.
18. **Concurrent KYC submissions** hit the unique index and returned 500. *Fix:* 409.
19. **Response typo:** reviewing with `REJECT` answered "KYC rejectd".
20. **Audit logging was best-effort after the fact:** a committed transfer
    could lack its audit row. *Fix:* audit rows written in the same transaction.
21. **Timestamps without time zone** (`TIMESTAMP(3)`). *Fix:* `timestamptz`, UTC on the wire.

## Moving existing data

The Go service uses its own schema, so existing data needs a one-off ETL
rather than an in-place migration:

1. `users`: map cuid → new UUID (keep a mapping table), lower-case emails, and
   carry bcrypt hashes in a `legacy_bcrypt` column, re-hashed to argon2id on next
   login (a small addition to `Hasher.Verify`). Alternatively, force a password reset.
2. `kyc_records`: copy as-is; map free-text `idType` onto the enum, flagging misfits for re-submission.
3. `wallets`: keep the account numbers.
4. **Balances:** for each wallet, post one `ADMIN_CREDIT` journal *ADJUSTMENTS →
   wallet* for the current balance ("Opening balance (migrated)"). History from
   the old `Ledger` table can be kept read-only in an archive table. It can't
   be replayed faithfully because its running balances were unreliable (defect 3).
5. Run `GET /admin/reconciliation` and compare each wallet's balance and total
   with the old system before switching traffic.
