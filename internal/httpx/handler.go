package httpx

import (
	"log/slog"
	"net/http"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

// HandlerFunc is an http.HandlerFunc that returns an error instead of
// rendering it, so every handler gets identical, centralised error handling.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Handle adapts fn to net/http, rendering any returned error with Error.
func Handle(log *slog.Logger, fn HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			Error(w, r, log, err)
		}
	}
}

// Principal returns the authenticated caller, or a 401 if the route was
// (mis)configured without the authentication middleware.
func Principal(r *http.Request) (reqctx.Principal, error) {
	p, ok := reqctx.PrincipalFrom(r.Context())
	if !ok {
		return p, apperr.Unauthorized(apperr.CodeUnauthorized, "Authentication required.")
	}
	return p, nil
}
