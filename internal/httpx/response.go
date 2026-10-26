// Package httpx is the shared HTTP toolkit: the response envelope, strict
// JSON decoding, validation, pagination and middleware.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

// Envelope is the success response shape shared with @irate/contracts:
// { "success": true, "message": "...", "data": ... }.
type Envelope struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// ErrorEnvelope is the failure shape:
// { "success": false, "message": "...", "code": "...", "errors": [...], "requestId": "..." }.
type ErrorEnvelope struct {
	Success   bool                `json:"success"`
	Message   string              `json:"message"`
	Code      string              `json:"code"`
	Errors    []apperr.FieldError `json:"errors,omitempty"`
	RequestID string              `json:"requestId,omitempty"`
}

// JSON writes v with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// OK writes a success envelope.
func OK(w http.ResponseWriter, status int, message string, data any) {
	JSON(w, status, Envelope{Success: true, Message: message, Data: data})
}

// RawOK writes a success envelope around already-encoded JSON data (used to
// replay a stored idempotent response byte-for-byte).
func RawOK(w http.ResponseWriter, status int, message string, data json.RawMessage) {
	JSON(w, status, Envelope{Success: true, Message: message, Data: data})
}

// Error renders err. *apperr.Error values are shown to the client as-is; any
// other error is logged in full and rendered as a generic 500, so internal
// details (SQL, stack traces, hostnames) never leak.
func Error(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	ctx := r.Context()
	requestID := reqctx.RequestID(ctx)

	ae, ok := apperr.As(err)
	switch {
	case ok:
		if ae.Status >= 500 {
			log.ErrorContext(ctx, "request failed", "err", err, "code", ae.Code, "request_id", requestID)
		}
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		// The client went away; there is nobody to answer. 499 is the
		// de-facto "client closed request" status (logs only).
		w.WriteHeader(499)
		return
	case errors.Is(err, context.DeadlineExceeded):
		log.WarnContext(ctx, "request timed out", "err", err, "request_id", requestID)
		ae = apperr.New(http.StatusServiceUnavailable, apperr.CodeTimeout, "The request took too long. Please try again.")
	default:
		log.ErrorContext(ctx, "unhandled error", "err", err, "request_id", requestID)
		ae = apperr.New(http.StatusInternalServerError, apperr.CodeInternal, "Something went wrong on our side. Please try again.")
	}

	if ae.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(ae.RetryAfter.Seconds()))))
	}
	JSON(w, ae.Status, ErrorEnvelope{
		Success:   false,
		Message:   ae.Message,
		Code:      ae.Code,
		Errors:    ae.Fields,
		RequestID: requestID,
	})
}
