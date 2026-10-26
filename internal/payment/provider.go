// Package payment integrates external payment providers (card / bank-transfer
// funding of wallets).
//
// Flow:
//
//  1. POST /payments/deposits creates a PENDING deposit with our own unique
//     reference and asks the provider for a checkout URL.
//  2. The customer pays on the provider's page.
//  3. The provider calls POST /webhooks/payments/{provider}. The signature is
//     verified, the event de-duplicated, and on success the wallet is credited
//     exactly once through a balanced ledger posting (SETTLEMENT -> wallet).
//
// Adding a real provider (Paystack, Flutterwave, Stripe) means implementing
// Provider; nothing else changes. See docs/PAYMENTS.md.
package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

var (
	// ErrInvalidSignature: the webhook was not signed by the provider.
	ErrInvalidSignature = errors.New("invalid webhook signature")
	// ErrMalformedEvent: the payload could not be understood.
	ErrMalformedEvent = errors.New("malformed webhook event")
)

// Provider is the contract every payment provider adapter implements.
type Provider interface {
	// Name is the stable identifier used in URLs and stored on deposits.
	Name() string
	// InitiateDeposit asks the provider to start collecting a payment.
	// Implementations must be safe to call again with the same reference
	// (providers de-duplicate by reference).
	InitiateDeposit(ctx context.Context, intent DepositIntent) (Checkout, error)
	// ParseWebhook authenticates and decodes an inbound webhook. It must
	// return ErrInvalidSignature unless the request provably came from the
	// provider.
	ParseWebhook(header http.Header, body []byte) (Event, error)
}

// Verifier is implemented by providers that can confirm a payment
// server-to-server. When available, every success webhook is verified before
// money is credited, and the verified amount is the one that counts: a
// forged or replayed webhook can never mint money on its own.
type Verifier interface {
	VerifyDeposit(ctx context.Context, reference string) (Verification, error)
}

type Verification struct {
	Succeeded bool
	Amount    int64 // minor units actually collected
	Currency  string
}

type DepositIntent struct {
	Reference     string
	Amount        int64 // minor units
	Currency      string
	CustomerEmail string
}

type Checkout struct {
	URL string
}

type EventOutcome int

const (
	OutcomeIgnored   EventOutcome = iota // an event type we do not act on
	OutcomeSucceeded                     // payment collected
	OutcomeFailed                        // payment definitively failed
)

// Event is a provider-neutral webhook event.
type Event struct {
	ID            string // provider's unique event id (de-duplication key)
	Type          string
	Outcome       EventOutcome
	Reference     string // our deposit reference
	Amount        int64  // minor units actually collected
	Currency      string
	FailureReason string
	Raw           json.RawMessage
}
