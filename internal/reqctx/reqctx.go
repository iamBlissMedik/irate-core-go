// Package reqctx carries request-scoped values (request id, client IP, the
// authenticated principal) through context.Context.
//
// It is a leaf package with no internal dependencies so that every layer —
// HTTP middleware, services, the audit log — can read these values without
// creating import cycles.
package reqctx

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	clientIPKey
	userAgentKey
	principalKey
)

// Role mirrors the database user_role enum.
type Role string

const (
	RoleUser  Role = "USER"
	RoleAdmin Role = "ADMIN"
)

// Principal is the authenticated caller, built from a verified access token.
type Principal struct {
	UserID    uuid.UUID
	Email     string
	Role      Role
	TokenID   string    // JWT "jti", used for revocation on logout
	IssuedAt  time.Time // JWT "iat"
	ExpiresAt time.Time // JWT "exp"
}

// IsAdmin reports whether the principal has the ADMIN role.
func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request id, or "" outside an HTTP request.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey, ip)
}

// ClientIP returns the resolved client IP, or "" outside an HTTP request.
func ClientIP(ctx context.Context) string {
	v, _ := ctx.Value(clientIPKey).(string)
	return v
}

func WithUserAgent(ctx context.Context, ua string) context.Context {
	return context.WithValue(ctx, userAgentKey, ua)
}

// UserAgent returns the caller's User-Agent, or "".
func UserAgent(ctx context.Context) string {
	v, _ := ctx.Value(userAgentKey).(string)
	return v
}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}
