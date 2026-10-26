package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

var (
	errMissingToken = apperr.Unauthorized(apperr.CodeUnauthorized, "Authentication required.")
	errExpiredToken = apperr.Unauthorized(apperr.CodeTokenExpired, "Your access token has expired.")
	errInvalidToken = apperr.Unauthorized(apperr.CodeTokenInvalid, "Invalid access token.")
	errRevokedToken = apperr.Unauthorized(apperr.CodeTokenInvalid, "This access token has been revoked. Please sign in again.")
)

// Authenticator verifies bearer tokens and attaches the principal.
type Authenticator struct {
	tokens      *TokenManager
	revocations *Revocations
	log         *slog.Logger
}

func NewAuthenticator(tokens *TokenManager, revocations *Revocations, log *slog.Logger) *Authenticator {
	return &Authenticator{tokens: tokens, revocations: revocations, log: log}
}

// Required rejects requests without a valid, unrevoked access token.
//
// If Redis cannot be reached the request FAILS CLOSED with 503: accepting a
// token we cannot check against the denylist could let a logged-out session
// keep moving money.
func (a *Authenticator) Required(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.authenticate(r)
		if err != nil {
			httpx.Error(w, r, a.log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(reqctx.WithPrincipal(r.Context(), p)))
	})
}

// Optional attaches the principal when a valid token is present and otherwise
// continues anonymously (used by logout, which must work with an expired token).
func (a *Authenticator) Optional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, err := a.authenticate(r); err == nil {
			r = r.WithContext(reqctx.WithPrincipal(r.Context(), p))
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Authenticator) authenticate(r *http.Request) (reqctx.Principal, error) {
	raw, ok := bearerToken(r)
	if !ok {
		return reqctx.Principal{}, errMissingToken
	}
	p, err := a.tokens.Verify(raw)
	switch {
	case errors.Is(err, ErrTokenExpired):
		return reqctx.Principal{}, errExpiredToken
	case err != nil:
		return reqctx.Principal{}, errInvalidToken
	}
	revoked, err := a.revocations.IsRevoked(r.Context(), p)
	if err != nil {
		return reqctx.Principal{}, apperr.Unavailable("Authentication is temporarily unavailable. Please retry.").WithCause(err)
	}
	if revoked {
		return reqctx.Principal{}, errRevokedToken
	}
	return p, nil
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != "" && !strings.ContainsAny(token, " \t")
}

// RequireRole allows only principals with the given role. Mount it after
// Authenticator.Required.
func RequireRole(role reqctx.Role, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := reqctx.PrincipalFrom(r.Context())
			if !ok {
				httpx.Error(w, r, log, errMissingToken)
				return
			}
			if p.Role != role {
				httpx.Error(w, r, log, apperr.Forbidden("You do not have permission to perform this action."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
