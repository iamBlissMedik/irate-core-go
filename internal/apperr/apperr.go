// Package apperr defines the typed errors that services return and the HTTP
// layer renders.
//
// Services never write HTTP responses. They return either an *Error (an
// expected, client-facing outcome such as "insufficient balance") or any other
// error, which is treated as an unexpected internal failure: logged in full,
// and shown to the client only as a generic 500 with the request id.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Machine-readable error codes. Clients should branch on Code, never on Message.
const (
	CodeValidation         = "VALIDATION_ERROR"
	CodeBadRequest         = "BAD_REQUEST"
	CodeUnsupportedMedia   = "UNSUPPORTED_MEDIA_TYPE"
	CodeMethodNotAllowed   = "METHOD_NOT_ALLOWED"
	CodePayloadTooLarge    = "PAYLOAD_TOO_LARGE"
	CodeUnauthorized       = "UNAUTHORIZED"
	CodeInvalidCredentials = "INVALID_CREDENTIALS" //nolint:gosec // an error code, not a credential
	CodeTokenExpired       = "ACCESS_TOKEN_EXPIRED"
	CodeTokenInvalid       = "INVALID_ACCESS_TOKEN"
	CodeRefreshInvalid     = "INVALID_REFRESH_TOKEN"
	CodeForbidden          = "FORBIDDEN"
	CodeNotFound           = "NOT_FOUND"
	CodeConflict           = "CONFLICT"
	CodeRateLimited        = "RATE_LIMITED"
	CodeAccountLocked      = "ACCOUNT_LOCKED"
	CodeUnavailable        = "SERVICE_UNAVAILABLE"
	CodeTimeout            = "REQUEST_TIMEOUT"
	CodeInternal           = "INTERNAL_ERROR"

	CodeEmailTaken           = "EMAIL_TAKEN"
	CodeWalletExists         = "WALLET_EXISTS"
	CodeWalletNotFound       = "WALLET_NOT_FOUND"
	CodeWalletFrozen         = "WALLET_FROZEN"
	CodeUnsupportedCurrency  = "UNSUPPORTED_CURRENCY"
	CodeCurrencyMismatch     = "CURRENCY_MISMATCH"
	CodeInsufficientFunds    = "INSUFFICIENT_FUNDS"
	CodeAmountTooLarge       = "AMOUNT_TOO_LARGE"
	CodeDailyLimitExceeded   = "DAILY_LIMIT_EXCEEDED"
	CodeSelfTransfer         = "SELF_TRANSFER"
	CodeRecipientNotFound    = "RECIPIENT_NOT_FOUND"
	CodeIdempotencyRequired  = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdempotencyMismatch  = "IDEMPOTENCY_KEY_REUSED"
	CodeIdempotencyInFlight  = "IDEMPOTENCY_KEY_IN_PROGRESS"
	CodeKYCPending           = "KYC_PENDING"
	CodeKYCAlreadyVerified   = "KYC_ALREADY_VERIFIED"
	CodeKYCNotFound          = "KYC_NOT_FOUND"
	CodeKYCAlreadyReviewed   = "KYC_ALREADY_REVIEWED"
	CodePaymentProviderError = "PAYMENT_PROVIDER_ERROR"
	CodeInvalidSignature     = "INVALID_SIGNATURE"
)

// FieldError describes one invalid request field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Error is an expected, client-facing failure.
type Error struct {
	Status     int
	Code       string
	Message    string
	Fields     []FieldError
	RetryAfter time.Duration // rendered as a Retry-After header when > 0
	cause      error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.cause }

// Is matches on Code, so errors.Is(err, apperr.ErrX) works even for copies
// produced by WithCause.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// WithCause returns a copy of e that wraps an underlying error for logging.
// The cause is never shown to clients.
func (e *Error) WithCause(cause error) *Error {
	cp := *e
	cp.cause = cause
	return &cp
}

// As extracts an *Error from err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func BadRequest(code, message string) *Error {
	return New(http.StatusBadRequest, code, message)
}

func Unauthorized(code, message string) *Error {
	return New(http.StatusUnauthorized, code, message)
}

func Forbidden(message string) *Error {
	return New(http.StatusForbidden, CodeForbidden, message)
}

func NotFound(code, message string) *Error {
	return New(http.StatusNotFound, code, message)
}

func Conflict(code, message string) *Error {
	return New(http.StatusConflict, code, message)
}

// Unprocessable is for well-formed requests that break a business rule
// (insufficient funds, frozen wallet, limits).
func Unprocessable(code, message string) *Error {
	return New(http.StatusUnprocessableEntity, code, message)
}

func TooManyRequests(code, message string, retryAfter time.Duration) *Error {
	e := New(http.StatusTooManyRequests, code, message)
	e.RetryAfter = retryAfter
	return e
}

func Unavailable(message string) *Error {
	return New(http.StatusServiceUnavailable, CodeUnavailable, message)
}

// Validation builds a 400 from one or more field errors. The top-level message
// joins them so simple clients can display it directly.
func Validation(fields ...FieldError) *Error {
	msg := "Validation failed"
	if len(fields) > 0 {
		msg = ""
		for i, f := range fields {
			if i > 0 {
				msg += ", "
			}
			msg += f.Field + ": " + f.Message
		}
	}
	return &Error{Status: http.StatusBadRequest, Code: CodeValidation, Message: msg, Fields: fields}
}

// Field is shorthand for a single-field validation error.
func Field(field, message string) *Error {
	return Validation(FieldError{Field: field, Message: message})
}
