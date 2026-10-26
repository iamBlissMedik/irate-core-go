package payment

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/audit"
	"github.com/iamBlissMedik/irate-core-go/internal/config"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/idempotency"
	"github.com/iamBlissMedik/irate-core-go/internal/ledger"
	"github.com/iamBlissMedik/irate-core-go/internal/money"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/metrics"
	"github.com/iamBlissMedik/irate-core-go/internal/wallet"
)

var ErrDepositNotFound = apperr.NotFound(apperr.CodeNotFound, "Deposit not found.")

type DepositRequest struct {
	Amount   money.Minor `json:"amount" validate:"gt=0"`
	Currency string      `json:"currency" validate:"omitempty,len=3,alpha"`
}

func (r *DepositRequest) Normalize() { r.Currency = strings.ToUpper(strings.TrimSpace(r.Currency)) }

// Deposit is the public deposit representation.
type Deposit struct {
	ID            uuid.UUID   `json:"id"`
	Reference     string      `json:"reference"`
	Provider      string      `json:"provider"`
	Amount        money.Minor `json:"amount"`
	Currency      string      `json:"currency"`
	Status        string      `json:"status"`
	CheckoutURL   *string     `json:"checkoutUrl"`
	FailureReason *string     `json:"failureReason"`
	TransactionID *uuid.UUID  `json:"transactionId"`
	CreatedAt     time.Time   `json:"createdAt"`
	CompletedAt   *time.Time  `json:"completedAt"`
}

func depositFromRow(d db.Deposit) Deposit {
	return Deposit{
		ID: d.ID, Reference: d.Reference, Provider: d.Provider, Amount: money.Minor(d.Amount),
		Currency: d.Currency, Status: string(d.Status), CheckoutURL: d.CheckoutUrl,
		FailureReason: d.FailureReason, TransactionID: d.TransactionID, CreatedAt: d.CreatedAt, CompletedAt: d.CompletedAt,
	}
}

type DepositList struct {
	Deposits   []Deposit      `json:"deposits"`
	Pagination httpx.PageMeta `json:"pagination"`
}

type Service struct {
	store     *database.Store
	providers map[string]Provider
	active    Provider
	money     config.MoneyConfig
	metrics   *metrics.Metrics
	log       *slog.Logger
}

// NewService registers providers; active is used for new deposits, while
// webhooks are accepted for every registered provider (so deposits started
// before a provider switch still settle).
func NewService(store *database.Store, active Provider, others []Provider, moneyCfg config.MoneyConfig, m *metrics.Metrics, log *slog.Logger) *Service {
	providers := map[string]Provider{active.Name(): active}
	for _, p := range others {
		providers[p.Name()] = p
	}
	return &Service{store: store, providers: providers, active: active, money: moneyCfg, metrics: m, log: log}
}

// InitiateDeposit starts (or resumes) a deposit. The Idempotency-Key maps to
// exactly one deposit: retrying returns the same deposit and reference, and if
// the provider call failed the first time, the retry re-attempts it with the
// same reference. The provider call happens OUTSIDE any database transaction,
// so a slow provider never holds locks or connections.
func (s *Service) InitiateDeposit(ctx context.Context, userID uuid.UUID, email string, req DepositRequest, idemKey string) (Deposit, bool, error) {
	if err := idempotency.ValidateKey(idemKey); err != nil {
		return Deposit{}, false, err
	}
	currency := req.Currency
	if currency == "" {
		currency = s.money.DefaultCurrency
	}
	if req.Amount.Int64() > s.money.MaxTransactionAmount {
		return Deposit{}, false, ledger.ErrAmountTooLarge
	}

	w, err := s.store.GetUserWallet(ctx, db.GetUserWalletParams{UserID: userID, Currency: currency})
	if database.IsNoRows(err) {
		return Deposit{}, false, wallet.ErrNotFound
	} else if err != nil {
		return Deposit{}, false, fmt.Errorf("get wallet: %w", err)
	}

	d, err := s.store.CreateDeposit(ctx, db.CreateDepositParams{
		ID: uuid.Must(uuid.NewV7()), UserID: userID, WalletID: w.ID, Provider: s.active.Name(),
		Reference: newDepositReference(), IdempotencyKey: idemKey, Amount: req.Amount.Int64(), Currency: currency,
	})
	created := err == nil
	if database.IsNoRows(err) {
		d, err = s.store.GetDepositByIdempotencyKey(ctx, db.GetDepositByIdempotencyKeyParams{UserID: userID, IdempotencyKey: idemKey})
		if err == nil && (d.Amount != req.Amount.Int64() || d.Currency != currency) {
			return Deposit{}, false, apperr.Unprocessable(apperr.CodeIdempotencyMismatch,
				"This Idempotency-Key was already used for a different deposit. Use a new key for a new deposit.")
		}
	}
	if err != nil {
		return Deposit{}, false, fmt.Errorf("create deposit: %w", err)
	}
	if d.CheckoutUrl != nil || d.Status != db.PaymentStatusPENDING {
		return depositFromRow(d), !created, nil
	}

	provider, ok := s.providers[d.Provider]
	if !ok {
		return Deposit{}, false, fmt.Errorf("deposit %s uses unregistered provider %q", d.ID, d.Provider)
	}
	checkout, err := provider.InitiateDeposit(ctx, DepositIntent{
		Reference: d.Reference, Amount: d.Amount, Currency: d.Currency, CustomerEmail: email,
	})
	if err != nil {
		return Deposit{}, false, apperr.New(http.StatusBadGateway, apperr.CodePaymentProviderError,
			"We could not start the payment with our provider. Please retry with the same Idempotency-Key.").WithCause(err)
	}
	d, err = s.store.SetDepositCheckoutURL(ctx, db.SetDepositCheckoutURLParams{ID: d.ID, CheckoutUrl: &checkout.URL})
	if err != nil {
		return Deposit{}, false, fmt.Errorf("save checkout url: %w", err)
	}
	if created {
		if err := audit.Record(ctx, s.store.Queries, audit.Entry{
			ActorID: &userID, Action: audit.ActionDepositInitiated, ResourceType: "deposit", ResourceID: d.ID.String(),
			Metadata: map[string]any{"reference": d.Reference, "amount": d.Amount, "currency": d.Currency, "provider": d.Provider},
		}); err != nil {
			s.log.WarnContext(ctx, "audit deposit initiated", "err", err)
		}
	}
	return depositFromRow(d), !created, nil
}

func (s *Service) Get(ctx context.Context, userID, depositID uuid.UUID) (Deposit, error) {
	d, err := s.store.GetDepositForUser(ctx, db.GetDepositForUserParams{ID: depositID, UserID: userID})
	if database.IsNoRows(err) {
		return Deposit{}, ErrDepositNotFound
	} else if err != nil {
		return Deposit{}, fmt.Errorf("get deposit: %w", err)
	}
	return depositFromRow(d), nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, p httpx.Page) (DepositList, error) {
	rows, err := s.store.ListUserDeposits(ctx, db.ListUserDepositsParams{UserID: userID, Lim: p.Lim(), Off: p.Offset()})
	if err != nil {
		return DepositList{}, fmt.Errorf("list deposits: %w", err)
	}
	total, err := s.store.CountUserDeposits(ctx, userID)
	if err != nil {
		return DepositList{}, fmt.Errorf("count deposits: %w", err)
	}
	out := DepositList{Deposits: make([]Deposit, 0, len(rows)), Pagination: httpx.NewPageMeta(total, p)}
	for _, d := range rows {
		out.Deposits = append(out.Deposits, depositFromRow(d))
	}
	return out, nil
}

// Webhook outcomes (metric label values).
const (
	webhookProcessed        = "processed"
	webhookDuplicate        = "duplicate"
	webhookIgnored          = "ignored"
	webhookUnknownReference = "unknown_reference"
	webhookAmountMismatch   = "amount_mismatch"
)

// HandleWebhook authenticates and applies a provider event. Returning nil
// tells the provider "received, stop retrying"; returning an error makes it
// retry later, so only transient failures (e.g. the database being down)
// produce errors. Every state change is a compare-and-set under a row lock,
// so concurrent or repeated deliveries credit a deposit at most once.
func (s *Service) HandleWebhook(ctx context.Context, providerName string, header http.Header, body []byte) error {
	provider, ok := s.providers[providerName]
	if !ok {
		return apperr.NotFound(apperr.CodeNotFound, "Unknown payment provider.")
	}
	ev, err := provider.ParseWebhook(header, body)
	switch {
	case errors.Is(err, ErrInvalidSignature):
		s.metrics.WebhookEvents.WithLabelValues(providerName, "invalid_signature").Inc()
		return apperr.Unauthorized(apperr.CodeInvalidSignature, "Invalid webhook signature.")
	case err != nil:
		s.metrics.WebhookEvents.WithLabelValues(providerName, "malformed").Inc()
		return apperr.BadRequest(apperr.CodeBadRequest, "Malformed webhook payload.")
	}

	// Confirm successes with the provider before touching the database (a
	// network call must never run inside a transaction). Errors are answered
	// with 502 and nothing is recorded, so the provider redelivers later.
	if v, ok := provider.(Verifier); ok && ev.Outcome == OutcomeSucceeded {
		res, err := v.VerifyDeposit(ctx, ev.Reference)
		if err != nil {
			s.metrics.WebhookEvents.WithLabelValues(providerName, "verification_error").Inc()
			return apperr.New(http.StatusBadGateway, apperr.CodePaymentProviderError,
				"Could not verify the payment with the provider; please redeliver.").WithCause(err)
		}
		if !res.Succeeded {
			s.log.WarnContext(ctx, "webhook reported success but provider verification did not",
				"provider", providerName, "reference", ev.Reference)
			s.metrics.WebhookEvents.WithLabelValues(providerName, "verification_failed").Inc()
			return apperr.New(http.StatusBadGateway, apperr.CodePaymentProviderError,
				"The provider did not confirm this payment.")
		}
		ev.Amount, ev.Currency = res.Amount, res.Currency // the verified figures are authoritative
	}

	var outcome string
	err = s.store.InTx(ctx, func(q *db.Queries) error {
		outcome = webhookProcessed // reset: InTx may re-run this closure after a deadlock
		eventRowID, err := q.RecordWebhookEvent(ctx, db.RecordWebhookEventParams{
			ID: uuid.Must(uuid.NewV7()), Provider: providerName, EventID: ev.ID, EventType: ev.Type, Payload: rawJSON(ev.Raw),
		})
		if database.IsNoRows(err) {
			outcome = webhookDuplicate
			return nil
		} else if err != nil {
			return fmt.Errorf("record webhook event: %w", err)
		}

		if ev.Outcome == OutcomeIgnored {
			outcome = webhookIgnored
			return q.MarkWebhookEventProcessed(ctx, eventRowID)
		}

		d, err := q.LockDepositByReference(ctx, db.LockDepositByReferenceParams{Provider: providerName, Reference: ev.Reference})
		if database.IsNoRows(err) {
			// Acknowledge (retrying will not help); the stored event row
			// keeps the payload for investigation.
			s.log.WarnContext(ctx, "webhook for unknown deposit reference", "provider", providerName, "reference", ev.Reference)
			outcome = webhookUnknownReference
			return q.MarkWebhookEventProcessed(ctx, eventRowID)
		} else if err != nil {
			return fmt.Errorf("lock deposit: %w", err)
		}

		switch ev.Outcome {
		case OutcomeSucceeded:
			if outcome, err = s.settle(ctx, q, d, ev); err != nil {
				return err
			}
		case OutcomeFailed:
			if err := s.fail(ctx, q, d, ev.FailureReason); err != nil {
				return err
			}
		}
		return q.MarkWebhookEventProcessed(ctx, eventRowID)
	})
	if err != nil {
		return err
	}
	s.metrics.WebhookEvents.WithLabelValues(providerName, outcome).Inc()
	return nil
}

// settle credits the wallet for a successful payment (deposit row locked).
func (s *Service) settle(ctx context.Context, q *db.Queries, d db.Deposit, ev Event) (string, error) {
	if d.Status == db.PaymentStatusSUCCEEDED {
		return webhookDuplicate, nil // already credited by an earlier event
	}
	if ev.Amount != d.Amount || !strings.EqualFold(ev.Currency, d.Currency) {
		// Never credit an amount we did not ask for: flag it for manual
		// review (the provider may have collected a different amount).
		s.log.ErrorContext(ctx, "deposit amount mismatch", "deposit_id", d.ID, "expected", d.Amount, "got", ev.Amount, "currency", ev.Currency)
		if d.Status == db.PaymentStatusPENDING {
			reason := fmt.Sprintf("amount_mismatch: expected %d %s, provider reported %d %s", d.Amount, d.Currency, ev.Amount, ev.Currency)
			if _, err := q.MarkDepositFailed(ctx, db.MarkDepositFailedParams{ID: d.ID, FailureReason: &reason}); err != nil {
				return "", fmt.Errorf("flag deposit: %w", err)
			}
		}
		return webhookAmountMismatch, audit.Record(ctx, q, audit.Entry{
			Action: audit.ActionDepositAmountFlag, ResourceType: "deposit", ResourceID: d.ID.String(),
			Metadata: map[string]any{"expected": d.Amount, "reported": ev.Amount, "currency": ev.Currency, "eventId": ev.ID},
		})
	}

	settlement, err := q.GetSystemWallet(ctx, db.GetSystemWalletParams{SystemCode: ledger.SystemSettlement, Currency: d.Currency})
	if err != nil {
		return "", fmt.Errorf("get %s account for %s: %w", ledger.SystemSettlement, d.Currency, err)
	}
	locked, err := ledger.Lock(ctx, q, d.WalletID, settlement.ID)
	if err != nil {
		return "", err
	}
	posted, err := ledger.Post(ctx, q, locked, ledger.Posting{
		Kind:        db.TransactionKindDEPOSIT,
		Currency:    d.Currency,
		Description: "Deposit " + d.Reference + " via " + d.Provider,
		Metadata:    map[string]any{"depositId": d.ID.String(), "provider": d.Provider, "providerEventId": ev.ID},
		Legs: []ledger.Leg{
			{WalletID: settlement.ID, Direction: db.EntryDirectionDEBIT, Amount: d.Amount, Description: "Deposit " + d.Reference},
			{WalletID: d.WalletID, Direction: db.EntryDirectionCREDIT, Amount: d.Amount, Description: "Wallet top-up"},
		},
	})
	if err != nil {
		return "", err
	}
	if _, err := q.MarkDepositSucceeded(ctx, db.MarkDepositSucceededParams{ID: d.ID, TransactionID: &posted.Transaction.ID}); err != nil {
		return "", fmt.Errorf("mark deposit succeeded: %w", err)
	}
	s.metrics.LedgerPostings.WithLabelValues(string(db.TransactionKindDEPOSIT), d.Currency).Inc()
	s.metrics.LedgerPostedMinor.WithLabelValues(string(db.TransactionKindDEPOSIT), d.Currency).Add(float64(d.Amount))
	return webhookProcessed, audit.Record(ctx, q, audit.Entry{
		ActorID: &d.UserID, Action: audit.ActionDepositSucceeded, ResourceType: "deposit", ResourceID: d.ID.String(),
		Metadata: map[string]any{"reference": d.Reference, "amount": d.Amount, "transactionReference": posted.Transaction.Reference},
	})
}

// fail marks a PENDING deposit as failed. Failure never overrides success.
func (s *Service) fail(ctx context.Context, q *db.Queries, d db.Deposit, reason string) error {
	if d.Status != db.PaymentStatusPENDING {
		return nil
	}
	if reason == "" {
		reason = "payment_failed"
	}
	if _, err := q.MarkDepositFailed(ctx, db.MarkDepositFailedParams{ID: d.ID, FailureReason: &reason}); err != nil {
		return fmt.Errorf("mark deposit failed: %w", err)
	}
	return audit.Record(ctx, q, audit.Entry{
		ActorID: &d.UserID, Action: audit.ActionDepositFailed, ResourceType: "deposit", ResourceID: d.ID.String(),
		Metadata: map[string]any{"reference": d.Reference, "reason": reason},
	})
}

// rawJSON guarantees a valid jsonb value even for an empty payload.
func rawJSON(b []byte) json.RawMessage {
	if json.Valid(b) {
		return b
	}
	return json.RawMessage(`{}`)
}

var refEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func newDepositReference() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "DEP-" + time.Now().UTC().Format("20060102") + "-" + refEncoding.EncodeToString(b[:])
}
