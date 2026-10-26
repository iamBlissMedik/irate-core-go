package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	PaystackName            = "paystack"
	PaystackSignatureHeader = "X-Paystack-Signature"
	PaystackDefaultBaseURL  = "https://api.paystack.co"
)

// Paystack integrates https://paystack.com (card, bank transfer and USSD
// collections in NGN, GHS, ZAR, KES, USD). Amounts are already in the
// currency's subunit (kobo), exactly like this service's minor units.
//
// Webhooks are authenticated with X-Paystack-Signature, the hex HMAC-SHA512 of
// the raw body keyed with the secret key. They carry no timestamp, so on top of
// the signature every success event is confirmed with the verify endpoint
// (see VerifyDeposit) before any money is credited.
type Paystack struct {
	secretKey   string
	baseURL     string
	callbackURL string
	http        *http.Client
}

// NewPaystack builds the adapter. baseURL is overridable for tests; client may
// be nil (a client with a 15s timeout is used).
func NewPaystack(secretKey, baseURL, callbackURL string, client *http.Client) *Paystack {
	if baseURL == "" {
		baseURL = PaystackDefaultBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Paystack{secretKey: secretKey, baseURL: strings.TrimRight(baseURL, "/"), callbackURL: callbackURL, http: client}
}

func (p *Paystack) Name() string { return PaystackName }

// paystackEnvelope is the shape of every Paystack API response.
type paystackEnvelope[T any] struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// InitiateDeposit calls POST /transaction/initialize and returns the hosted
// checkout (authorization_url).
func (p *Paystack) InitiateDeposit(ctx context.Context, in DepositIntent) (Checkout, error) {
	body := map[string]any{
		"email":     in.CustomerEmail,
		"amount":    in.Amount,
		"currency":  in.Currency,
		"reference": in.Reference,
	}
	if p.callbackURL != "" {
		body["callback_url"] = p.callbackURL
	}
	var out paystackEnvelope[struct {
		AuthorizationURL string `json:"authorization_url"`
		Reference        string `json:"reference"`
	}]
	if err := p.call(ctx, http.MethodPost, "/transaction/initialize", body, &out); err != nil {
		return Checkout{}, err
	}
	if out.Data.AuthorizationURL == "" {
		return Checkout{}, errors.New("paystack: initialize returned no authorization_url")
	}
	return Checkout{URL: out.Data.AuthorizationURL}, nil
}

// VerifyDeposit calls GET /transaction/verify/:reference, Paystack's
// authoritative answer on whether (and how much) money was collected.
func (p *Paystack) VerifyDeposit(ctx context.Context, reference string) (Verification, error) {
	var out paystackEnvelope[struct {
		Status   string `json:"status"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}]
	if err := p.call(ctx, http.MethodGet, "/transaction/verify/"+url.PathEscape(reference), nil, &out); err != nil {
		return Verification{}, err
	}
	return Verification{
		Succeeded: out.Data.Status == "success",
		Amount:    out.Data.Amount,
		Currency:  strings.ToUpper(out.Data.Currency),
	}, nil
}

// paystackWebhook is the subset of a webhook body we need.
type paystackWebhook struct {
	Event string `json:"event"`
	Data  struct {
		ID              int64  `json:"id"`
		Status          string `json:"status"`
		Reference       string `json:"reference"`
		Amount          int64  `json:"amount"`
		Currency        string `json:"currency"`
		GatewayResponse string `json:"gateway_response"`
	} `json:"data"`
}

func (p *Paystack) ParseWebhook(h http.Header, body []byte) (Event, error) {
	given, err := hex.DecodeString(h.Get(PaystackSignatureHeader))
	if err != nil || len(given) == 0 {
		return Event{}, ErrInvalidSignature
	}
	mac := hmac.New(sha512.New, []byte(p.secretKey))
	mac.Write(body)
	if !hmac.Equal(given, mac.Sum(nil)) {
		return Event{}, ErrInvalidSignature
	}

	var w paystackWebhook
	if err := json.Unmarshal(body, &w); err != nil || w.Event == "" || w.Data.Reference == "" {
		return Event{}, ErrMalformedEvent
	}
	// Paystack events have no top-level id; the transaction id plus the event
	// name identifies a delivery (redeliveries repeat both).
	id := fmt.Sprintf("%s:%d", w.Event, w.Data.ID)
	if w.Data.ID == 0 {
		id = w.Event + ":" + w.Data.Reference
	}
	ev := Event{
		ID: id, Type: w.Event, Reference: w.Data.Reference, Amount: w.Data.Amount,
		Currency: strings.ToUpper(w.Data.Currency), FailureReason: w.Data.GatewayResponse, Raw: body,
	}
	if w.Event == "charge.success" && w.Data.Status == "success" {
		ev.Outcome = OutcomeSucceeded
	}
	// Everything else (transfers, refunds, disputes, subscriptions) is
	// acknowledged and ignored. An abandoned checkout sends no event; the
	// deposit simply stays PENDING.
	return ev, nil
}

// SignForTest signs body the way Paystack does (for tests and local tooling).
func (p *Paystack) SignForTest(body []byte) http.Header {
	mac := hmac.New(sha512.New, []byte(p.secretKey))
	mac.Write(body)
	h := http.Header{}
	h.Set(PaystackSignatureHeader, hex.EncodeToString(mac.Sum(nil)))
	h.Set("Content-Type", "application/json")
	return h
}

func (p *Paystack) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("paystack: marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("paystack: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.secretKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("paystack: %s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("paystack: read response: %w", err)
	}

	var env paystackEnvelope[json.RawMessage]
	_ = json.Unmarshal(raw, &env) // best effort: used for the error message
	if res.StatusCode >= 300 || !env.Status {
		msg := env.Message
		if msg == "" {
			msg = http.StatusText(res.StatusCode)
		}
		return fmt.Errorf("paystack: %s %s: HTTP %d: %s", method, path, res.StatusCode, msg)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("paystack: decode response: %w", err)
	}
	return nil
}
