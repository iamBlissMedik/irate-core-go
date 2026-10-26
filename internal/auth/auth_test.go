package auth

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

const (
	secretA = "test-secret-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	secretB = "test-secret-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func newTM(secret, previous string) *TokenManager {
	return NewTokenManager(secret, previous, "irate-core", "irate-api", 15*time.Minute)
}

func TestTokenRoundTrip(t *testing.T) {
	tm := newTM(secretA, "")
	uid := uuid.New()
	raw, claims, err := tm.Issue(uid, "ada@irate.dev", reqctx.RoleAdmin)
	require.NoError(t, err)

	p, err := tm.Verify(raw)
	require.NoError(t, err)
	assert.Equal(t, uid, p.UserID)
	assert.Equal(t, reqctx.RoleAdmin, p.Role)
	assert.Equal(t, "ada@irate.dev", p.Email)
	assert.Equal(t, claims.ID, p.TokenID)
	assert.WithinDuration(t, time.Now().Add(15*time.Minute), p.ExpiresAt, 2*time.Second)
}

func TestTokenExpired(t *testing.T) {
	tm := newTM(secretA, "")
	raw, _, err := tm.Issue(uuid.New(), "a@b.co", reqctx.RoleUser)
	require.NoError(t, err)
	tm.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	_, err = tm.Verify(raw)
	assert.ErrorIs(t, err, ErrTokenExpired)
}

func TestTokenRejectsForgeries(t *testing.T) {
	tm := newTM(secretA, "")
	good, _, err := tm.Issue(uuid.New(), "a@b.co", reqctx.RoleUser)
	require.NoError(t, err)

	other, _, err := newTM(secretB, "").Issue(uuid.New(), "a@b.co", reqctx.RoleUser)
	require.NoError(t, err)

	// alg=none with an escalated role.
	parts := strings.Split(good, ".")
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	escalated := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(payload), `"USER"`, `"ADMIN"`, 1)))

	wrongAud := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{Role: "USER", RegisteredClaims: jwt.RegisteredClaims{
		Subject: uuid.NewString(), Issuer: "irate-core", Audience: jwt.ClaimStrings{"someone-else"}, ID: "x",
		IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}})
	wrongAud.Header["kid"] = keyID(secretA)
	wrongAudRaw, _ := wrongAud.SignedString([]byte(secretA))

	for name, raw := range map[string]string{
		"signed with another key": other,
		"alg none":                noneHeader + "." + escalated + ".",
		"payload tampered":        parts[0] + "." + escalated + "." + parts[2],
		"wrong audience":          wrongAudRaw,
		"garbage":                 "not.a.jwt",
		"empty":                   "",
	} {
		_, err := tm.Verify(raw)
		assert.ErrorIs(t, err, ErrTokenInvalid, name)
	}
}

func TestTokenKeyRotation(t *testing.T) {
	oldToken, _, err := newTM(secretA, "").Issue(uuid.New(), "a@b.co", reqctx.RoleUser)
	require.NoError(t, err)

	rotated := newTM(secretB, secretA) // new current key, old key still accepted
	_, err = rotated.Verify(oldToken)
	assert.NoError(t, err, "tokens signed with the previous key stay valid during rotation")

	_, err = newTM(secretB, "").Verify(oldToken)
	assert.ErrorIs(t, err, ErrTokenInvalid, "once the previous key is removed, its tokens die")
}

func TestRefreshTokens(t *testing.T) {
	tok, hash, err := NewRefreshToken()
	require.NoError(t, err)
	assert.Len(t, tok, 43, "32 bytes, base64url without padding")
	assert.Equal(t, hash, HashRefreshToken(tok))
	tok2, _, _ := NewRefreshToken()
	assert.NotEqual(t, tok, tok2)
}

func TestPasswordHashing(t *testing.T) {
	h := NewHasher(2)
	ctx := context.Background()
	enc, err := h.Hash(ctx, "Password123!")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(enc, "$argon2id$v=19$m=19456,t=2,p=1$"))

	ok, err := h.Verify(ctx, "Password123!", enc)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = h.Verify(ctx, "password123!", enc)
	require.NoError(t, err)
	assert.False(t, ok)

	enc2, _ := h.Hash(ctx, "Password123!")
	assert.NotEqual(t, enc, enc2, "random salt per hash")

	_, err = h.Verify(ctx, "x", "$2b$10$notargon")
	assert.Error(t, err)

	assert.False(t, h.NeedsRehash(enc))
	assert.True(t, h.NeedsRehash("$argon2id$v=19$m=4096,t=1,p=1$c2FsdA$a2V5"))
}

func TestHasherRespectsContextWhenSaturated(t *testing.T) {
	h := NewHasher(1)
	h.sem <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := h.Hash(ctx, "Password123!")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	return mr, redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

func TestLockout(t *testing.T) {
	mr, rdb := newRedis(t)
	l := NewLockout(rdb, 3, 15*time.Minute)
	ctx := context.Background()

	for range 2 {
		require.NoError(t, l.Fail(ctx, "ada@irate.dev"))
	}
	locked, _, err := l.Locked(ctx, "ada@irate.dev")
	require.NoError(t, err)
	assert.False(t, locked)

	require.NoError(t, l.Fail(ctx, "ada@irate.dev"))
	locked, retry, err := l.Locked(ctx, "ada@irate.dev")
	require.NoError(t, err)
	assert.True(t, locked)
	assert.InDelta(t, (15 * time.Minute).Seconds(), retry.Seconds(), 1)

	other, _, _ := l.Locked(ctx, "bola@irate.dev")
	assert.False(t, other, "lockout is per email")

	mr.FastForward(16 * time.Minute)
	locked, _, _ = l.Locked(ctx, "ada@irate.dev")
	assert.False(t, locked, "lock expires")

	require.NoError(t, l.Fail(ctx, "ada@irate.dev"))
	require.NoError(t, l.Reset(ctx, "ada@irate.dev"))
	assert.False(t, mr.Exists(lockoutKey("ada@irate.dev")))
	for _, k := range mr.Keys() {
		assert.NotContains(t, k, "ada", "emails are never stored in plain text")
	}
}

func TestRevocations(t *testing.T) {
	_, rdb := newRedis(t)
	r := NewRevocations(rdb)
	ctx := context.Background()
	uid := uuid.New()
	p := reqctx.Principal{UserID: uid, TokenID: "jti-1", IssuedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(10 * time.Minute)}

	revoked, err := r.IsRevoked(ctx, p)
	require.NoError(t, err)
	assert.False(t, revoked)

	require.NoError(t, r.RevokeToken(ctx, p))
	revoked, _ = r.IsRevoked(ctx, p)
	assert.True(t, revoked)

	p2 := p
	p2.TokenID = "jti-2"
	revoked, _ = r.IsRevoked(ctx, p2)
	assert.False(t, revoked, "only the logged-out token is revoked")

	require.NoError(t, r.RevokeUserTokens(ctx, uid.String(), 15*time.Minute))
	revoked, _ = r.IsRevoked(ctx, p2)
	assert.True(t, revoked, "logout-all revokes older tokens")

	fresh := p2
	fresh.TokenID, fresh.IssuedAt = "jti-3", time.Now().Add(5*time.Millisecond)
	revoked, _ = r.IsRevoked(ctx, fresh)
	assert.False(t, revoked, "a token issued right after logout-all is valid")
}

func TestAuthenticator(t *testing.T) {
	mr, rdb := newRedis(t)
	tm := newTM(secretA, "")
	revs := NewRevocations(rdb)
	a := NewAuthenticator(tm, revs, discard)

	var got reqctx.Principal
	protected := a.Required(RequireRole(reqctx.RoleAdmin, discard)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = reqctx.PrincipalFrom(r.Context())
	})))
	call := func(header string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		protected.ServeHTTP(rec, req)
		return rec.Code
	}

	admin, _, _ := tm.Issue(uuid.New(), "admin@irate.dev", reqctx.RoleAdmin)
	user, _, _ := tm.Issue(uuid.New(), "ada@irate.dev", reqctx.RoleUser)

	assert.Equal(t, http.StatusUnauthorized, call(""))
	assert.Equal(t, http.StatusUnauthorized, call("Basic abc"))
	assert.Equal(t, http.StatusUnauthorized, call("Bearer garbage"))
	assert.Equal(t, http.StatusForbidden, call("Bearer "+user))
	assert.Equal(t, http.StatusOK, call("bearer "+admin), "scheme is case-insensitive")
	assert.Equal(t, reqctx.RoleAdmin, got.Role)

	p, _ := tm.Verify(admin)
	require.NoError(t, revs.RevokeToken(context.Background(), p))
	assert.Equal(t, http.StatusUnauthorized, call("Bearer "+admin))

	mr.Close() // Redis down: authentication must fail closed
	assert.Equal(t, http.StatusServiceUnavailable, call("Bearer "+user))
}
