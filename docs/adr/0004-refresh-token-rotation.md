# ADR 0004: Rotating refresh tokens with reuse detection

**Status:** Accepted

## Context

Express issued a 7-day JWT refresh token and stored one per user in Redis
(`refresh:<userId>`):

- logging in on a second device silently logged out the first;
- refresh tokens were never rotated, so a stolen one worked for 7 days;
- the role inside the refresh JWT was copied into new access tokens forever;
- logout required a valid access token, so an expired session could not
  revoke its refresh token.

## Decision

- Refresh tokens are **opaque 256-bit random strings**, stored as SHA-256 hashes
  in Postgres (`refresh_tokens`), one row per issued token, grouped in a
  **family** per login.
- Every refresh **rotates**: the presented row gets `rotated_at`, and a new row joins
  the family. The row is locked `FOR UPDATE`, so concurrent refreshes serialise.
- Presenting an already-rotated token:
  - within `REFRESH_REUSE_GRACE` (20 s): 401, family kept. Two browser tabs
    racing is normal; the BFF also single-flights refreshes.
  - later: **revoke the whole family** and audit `REFRESH_TOKEN_REUSE_DETECTED`.
    Either the thief or the victim used an old token; both are forced to log in.
- The user (and role) is re-read from the database on refresh.
- Logout accepts the refresh cookie alone. Logout-all revokes all families
  and sets a per-user access-token cutoff in Redis (millisecond precision, so an
  immediate re-login is not caught by its own cutoff).
- Access tokens stay stateless JWTs (15 min), with a Redis denylist for
  explicit logout.

## Consequences

- Multi-device sessions work; each device is its own family.
- A stolen refresh token is good for at most one use before detection.
- One Postgres write per refresh (cheap: every 15 minutes per active session).
