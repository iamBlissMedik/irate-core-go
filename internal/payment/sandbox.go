package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	SandboxName            = "sandbox"
	SandboxSignatureHeader = "X-Sandbox-Signature"
	SandboxTimestampHeader = "X-Sandbox-Timestamp"
	sandboxTolerance       = 5 * time.Minute
)

// Sandbox is a self-contained provider for development and tests. It behaves
// like a real provider at the webhook boundary: events are HMAC-SHA256 signed
// over "<unix timestamp>.<body>" (the scheme Stripe uses), and stale
// timestamps are rejected so a captured webhook cannot be replayed later.
type Sandbox struct {
	secret  []byte
	baseURL string
	now     func() time.Time
}

func NewSandbox(secret, publicBaseURL string) *Sandbox {
	return &Sandbox{secret: []byte(secret), baseURL: publicBaseURL, now: time.Now}
}

func (s *Sandbox) Name() string { return SandboxName }

// InitiateDeposit returns an informational checkout URL. The sandbox has no
// hosted payment page; complete payments with the simulate endpoint.
func (s *Sandbox) InitiateDeposit(_ context.Context, in DepositIntent) (Checkout, error) {
	return Checkout{URL: s.baseURL + "/sandbox/checkout?reference=" + url.QueryEscape(in.Reference)}, nil
}

// SandboxPayload is the sandbox webhook body.
type SandboxPayload struct {
	ID   string `json:"id"`
	Type string `json:"type"` // payment.succeeded | payment.failed
	Data struct {
		Reference     string `json:"reference"`
		Amount        int64  `json:"amount"`
		Currency      string `json:"currency"`
		FailureReason string `json:"failureReason,omitempty"`
	} `json:"data"`
}

func (s *Sandbox) ParseWebhook(h http.Header, body []byte) (Event, error) {
	ts, err := strconv.ParseInt(h.Get(SandboxTimestampHeader), 10, 64)
	if err != nil {
		return Event{}, ErrInvalidSignature
	}
	if age := s.now().Sub(time.Unix(ts, 0)); age > sandboxTolerance || age < -sandboxTolerance {
		return Event{}, ErrInvalidSignature
	}
	given, err := hex.DecodeString(h.Get(SandboxSignatureHeader))
	if err != nil || !hmac.Equal(given, s.sign(ts, body)) { // constant-time comparison
		return Event{}, ErrInvalidSignature
	}

	var p SandboxPayload
	if err := json.Unmarshal(body, &p); err != nil || p.ID == "" || p.Data.Reference == "" {
		return Event{}, ErrMalformedEvent
	}
	ev := Event{
		ID: p.ID, Type: p.Type, Reference: p.Data.Reference, Amount: p.Data.Amount,
		Currency: p.Data.Currency, FailureReason: p.Data.FailureReason, Raw: body,
	}
	switch p.Type {
	case "payment.succeeded":
		ev.Outcome = OutcomeSucceeded
	case "payment.failed":
		ev.Outcome = OutcomeFailed
	default:
		ev.Outcome = OutcomeIgnored
	}
	return ev, nil
}

// SignedRequestHeaders signs body as the sandbox "provider" would. Used by the
// simulate endpoint and by tests.
func (s *Sandbox) SignedRequestHeaders(body []byte) http.Header {
	ts := s.now().Unix()
	h := http.Header{}
	h.Set(SandboxTimestampHeader, strconv.FormatInt(ts, 10))
	h.Set(SandboxSignatureHeader, hex.EncodeToString(s.sign(ts, body)))
	h.Set("Content-Type", "application/json")
	return h
}

func (s *Sandbox) sign(ts int64, body []byte) []byte {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = fmt.Fprintf(mac, "%d.", ts) // hash.Hash writes never fail
	mac.Write(body)
	return mac.Sum(nil)
}
