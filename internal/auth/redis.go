package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

// Lockout counts failed logins per email and blocks further attempts once the
// limit is reached, for a sliding window after the most recent failure.
//
// Only FAILURES count (the original implementation counted every attempt,
// including successful ones). Emails are hashed in the key so Redis never
// holds a list of customer emails.
type Lockout struct {
	rdb         *redis.Client
	maxAttempts int64
	window      time.Duration
}

func NewLockout(rdb *redis.Client, maxAttempts int, window time.Duration) *Lockout {
	return &Lockout{rdb: rdb, maxAttempts: int64(maxAttempts), window: window}
}

func lockoutKey(email string) string {
	sum := sha256.Sum256([]byte(email))
	return "auth:login-failures:" + hex.EncodeToString(sum[:16])
}

// Locked reports whether email is locked out and for how much longer.
func (l *Lockout) Locked(ctx context.Context, email string) (bool, time.Duration, error) {
	key := lockoutKey(email)
	n, err := l.rdb.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if n < l.maxAttempts {
		return false, 0, nil
	}
	ttl, err := l.rdb.PTTL(ctx, key).Result()
	if err != nil || ttl < 0 {
		// A counter without an expiry would lock the account forever (only
		// possible if the key was written outside this code); heal it.
		ttl = l.window
		_ = l.rdb.PExpire(ctx, key, l.window).Err()
	}
	return true, ttl, nil
}

// Fail records a failed attempt. INCR and EXPIRE run in one MULTI, so a crash
// can never leave a counter without a TTL (which would lock the account forever).
func (l *Lockout) Fail(ctx context.Context, email string) error {
	key := lockoutKey(email)
	_, err := l.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Incr(ctx, key)
		p.PExpire(ctx, key, l.window)
		return nil
	})
	return err
}

// Reset clears the counter after a successful login.
func (l *Lockout) Reset(ctx context.Context, email string) error {
	return l.rdb.Del(ctx, lockoutKey(email)).Err()
}

// Revocations is the access-token denylist.
//
// Access tokens are stateless JWTs, so logout cannot delete them; instead their
// id ("jti") is denylisted until the token would have expired anyway. "Log out
// everywhere" stores a per-user cutoff: tokens issued at or before it are
// rejected. Both checks are one MGET per authenticated request.
type Revocations struct {
	rdb *redis.Client
}

func NewRevocations(rdb *redis.Client) *Revocations { return &Revocations{rdb: rdb} }

func tokenKey(jti string) string   { return "auth:revoked:token:" + jti }
func userKey(userID string) string { return "auth:revoked:user:" + userID }

// RevokeToken denylists one access token until it expires.
func (r *Revocations) RevokeToken(ctx context.Context, p reqctx.Principal) error {
	ttl := time.Until(p.ExpiresAt) + 5*time.Second // cover verification leeway
	if ttl <= 0 {
		return nil
	}
	return r.rdb.Set(ctx, tokenKey(p.TokenID), "1", ttl).Err()
}

// RevokeUserTokens rejects every access token issued to the user up to now
// (millisecond precision, so logging straight back in works). The marker only
// needs to outlive the longest-lived access token.
func (r *Revocations) RevokeUserTokens(ctx context.Context, userID string, accessTTL time.Duration) error {
	return r.rdb.Set(ctx, userKey(userID), strconv.FormatInt(time.Now().UnixMilli(), 10), accessTTL+time.Minute).Err()
}

// IsRevoked checks both denylists.
func (r *Revocations) IsRevoked(ctx context.Context, p reqctx.Principal) (bool, error) {
	vals, err := r.rdb.MGet(ctx, tokenKey(p.TokenID), userKey(p.UserID.String())).Result()
	if err != nil {
		return false, fmt.Errorf("check token revocation: %w", err)
	}
	if vals[0] != nil {
		return true, nil
	}
	if s, ok := vals[1].(string); ok {
		cutoff, err := strconv.ParseInt(s, 10, 64)
		if err == nil && p.IssuedAt.UnixMilli() <= cutoff {
			return true, nil
		}
	}
	return false, nil
}
