// Package idempotency makes retried mutations safe.
//
// A client sends a unique Idempotency-Key with every money-moving request. If
// the network drops the response and the client retries with the same key,
// the second request must not move money again — it must get the original
// response back.
//
// The key is claimed INSIDE the same database transaction as the work it
// protects:
//
//	BEGIN
//	  Claim(key)            -- INSERT ... ON CONFLICT DO NOTHING
//	  ...move money...
//	  Complete(key, resp)   -- store the response
//	COMMIT
//
// Properties this gives, without any distributed lock:
//   - Atomic: the money movement and the key record commit (or roll back)
//     together. There is no window where money moved but the key was lost.
//   - Concurrency-safe: a concurrent duplicate blocks on the primary key until
//     the first transaction ends, then sees the committed record and replays.
//   - Failures are not cached: a rejected request (e.g. insufficient funds)
//     rolls back its claim, so the client may retry after topping up.
//   - Scoped per user: one customer can never replay another's response.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
)

// Header is the request header carrying the key.
const Header = "Idempotency-Key"

// ReplayedHeader is set on responses served from a stored result.
const ReplayedHeader = "Idempotent-Replayed"

const maxKeyLength = 255

// Scopes namespace keys per operation, so the same key value used for a
// transfer and for a deposit does not collide.
const (
	ScopeTransfer    = "transfer"
	ScopeAdminCredit = "admin_credit"
)

// Key identifies one idempotent request.
type Key struct {
	UserID      uuid.UUID
	Scope       string
	Value       string
	RequestHash string
}

// Replay is a stored response to return instead of re-executing.
type Replay struct {
	Status int
	Body   json.RawMessage
}

// ValidateKey checks a raw header value.
func ValidateKey(raw string) error {
	if raw == "" {
		return apperr.BadRequest(apperr.CodeIdempotencyRequired,
			"The Idempotency-Key header is required for this request (use a fresh UUID per operation).")
	}
	if len(raw) > maxKeyLength {
		return apperr.BadRequest(apperr.CodeBadRequest, fmt.Sprintf("Idempotency-Key must be at most %d characters", maxKeyLength))
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c < 0x21 || c > 0x7e {
			return apperr.BadRequest(apperr.CodeBadRequest, "Idempotency-Key must contain only visible ASCII characters")
		}
	}
	return nil
}

// HashRequest fingerprints the semantically relevant request fields, so a key
// reused with a DIFFERENT payload is rejected instead of silently replaying a
// response for some other request.
func HashRequest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0x1f}) // unit separator: ("ab","c") != ("a","bc")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Claim must be the first statement of the protecting transaction. It returns
// (nil, nil) when this request is first and should execute, or a Replay when
// the key was already completed.
func Claim(ctx context.Context, q *db.Queries, k Key) (*Replay, error) {
	_, err := q.ClaimIdempotencyKey(ctx, db.ClaimIdempotencyKeyParams{
		UserID: k.UserID, Scope: k.Scope, Key: k.Value, RequestHash: k.RequestHash,
	})
	if err == nil {
		return nil, nil
	}
	if !database.IsNoRows(err) {
		return nil, fmt.Errorf("claim idempotency key: %w", err)
	}

	existing, err := q.GetIdempotencyKey(ctx, db.GetIdempotencyKeyParams{UserID: k.UserID, Scope: k.Scope, Key: k.Value})
	if err != nil {
		return nil, fmt.Errorf("load idempotency key: %w", err)
	}
	if existing.RequestHash != k.RequestHash {
		return nil, apperr.Unprocessable(apperr.CodeIdempotencyMismatch,
			"This Idempotency-Key was already used for a different request. Use a new key for a new operation.")
	}
	if existing.ResponseStatus == 0 || existing.ResponseBody == nil {
		// Unreachable while Claim and Complete share a transaction (the
		// record only becomes visible on commit, already completed); kept
		// as a defensive answer rather than replaying an empty body.
		return nil, apperr.Conflict(apperr.CodeIdempotencyInFlight, "A request with this Idempotency-Key is still being processed.")
	}
	return &Replay{Status: int(existing.ResponseStatus), Body: existing.ResponseBody}, nil
}

// Complete stores the response for future replays. Call it in the same
// transaction as Claim, after the work succeeded.
func Complete(ctx context.Context, q *db.Queries, k Key, status int, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal idempotent response: %w", err)
	}
	if status == 0 {
		status = http.StatusOK
	}
	return q.CompleteIdempotencyKey(ctx, db.CompleteIdempotencyKeyParams{
		ResponseStatus: int32(status), ResponseBody: raw,
		UserID: k.UserID, Scope: k.Scope, Key: k.Value,
	})
}
