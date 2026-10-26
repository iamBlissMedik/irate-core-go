package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

func init() {
	// Encode iat/exp with millisecond precision (RFC 7519 NumericDate may be
	// fractional). "Log out everywhere" compares iat against a cutoff; with
	// whole seconds, a token issued in the same second as the cutoff, i.e. an
	// immediate re-login, would be wrongly rejected.
	jwt.TimePrecision = time.Millisecond
}

var (
	ErrTokenExpired = errors.New("access token expired")
	ErrTokenInvalid = errors.New("access token invalid")
)

// Claims are the access-token claims. "email" and "role" are read by the BFF
// to render the session without a round-trip; "sub" is the user id.
type Claims struct {
	Email string `json:"email"`
	Role  string `json:"role"`
	jwt.RegisteredClaims
}

// TokenManager issues and verifies short-lived HS256 access tokens.
//
// Each signing key gets a key id ("kid") derived from the secret. Setting
// JWT_ACCESS_SECRET_PREVIOUS to the old secret during a rotation keeps tokens
// signed with it valid until they expire, so rotating the key logs nobody out.
type TokenManager struct {
	keys       map[string][]byte
	currentKID string
	issuer     string
	audience   string
	ttl        time.Duration
	parser     *jwt.Parser
	now        func() time.Time
}

func NewTokenManager(secret, previousSecret, issuer, audience string, ttl time.Duration) *TokenManager {
	tm := &TokenManager{
		keys:     map[string][]byte{},
		issuer:   issuer,
		audience: audience,
		ttl:      ttl,
		now:      time.Now,
	}
	tm.currentKID = keyID(secret)
	tm.keys[tm.currentKID] = []byte(secret)
	if previousSecret != "" {
		tm.keys[keyID(previousSecret)] = []byte(previousSecret)
	}
	tm.parser = jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), // blocks alg=none / alg confusion
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(5*time.Second),
		jwt.WithTimeFunc(func() time.Time { return tm.now() }),
	)
	return tm
}

func keyID(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:4])
}

// TTL is the access-token lifetime.
func (tm *TokenManager) TTL() time.Duration { return tm.ttl }

// Issue signs an access token for the user.
func (tm *TokenManager) Issue(userID uuid.UUID, email string, role reqctx.Role) (string, Claims, error) {
	now := tm.now()
	claims := Claims{
		Email: email,
		Role:  string(role),
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.Must(uuid.NewV7()).String(),
			Subject:   userID.String(),
			Issuer:    tm.issuer,
			Audience:  jwt.ClaimStrings{tm.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tm.ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = tm.currentKID
	signed, err := tok.SignedString(tm.keys[tm.currentKID])
	if err != nil {
		return "", Claims{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, claims, nil
}

// Verify checks signature, algorithm, issuer, audience and time claims, and
// returns the principal. It returns ErrTokenExpired or ErrTokenInvalid.
func (tm *TokenManager) Verify(raw string) (reqctx.Principal, error) {
	var claims Claims
	_, err := tm.parser.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key, ok := tm.keys[kid]
		if !ok {
			return nil, errors.New("unknown signing key")
		}
		return key, nil
	})
	if errors.Is(err, jwt.ErrTokenExpired) {
		return reqctx.Principal{}, ErrTokenExpired
	}
	if err != nil {
		return reqctx.Principal{}, ErrTokenInvalid
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil || claims.ID == "" || claims.IssuedAt == nil {
		return reqctx.Principal{}, ErrTokenInvalid
	}
	role := reqctx.Role(claims.Role)
	if role != reqctx.RoleUser && role != reqctx.RoleAdmin {
		return reqctx.Principal{}, ErrTokenInvalid
	}
	return reqctx.Principal{
		UserID:    userID,
		Email:     claims.Email,
		Role:      role,
		TokenID:   claims.ID,
		IssuedAt:  claims.IssuedAt.Time,
		ExpiresAt: claims.ExpiresAt.Time,
	}, nil
}

// NewRefreshToken returns an opaque, URL-safe refresh token (256 random bits)
// and the SHA-256 hash that is stored instead of the token itself.
func NewRefreshToken() (token string, hash []byte, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, fmt.Errorf("generate refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken hashes a presented token for lookup. A fast hash is right
// here (unlike passwords): the input has 256 bits of entropy, so brute force
// is not a concern.
func HashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
