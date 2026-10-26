package payment

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/idempotency"
)

type Handler struct {
	svc     *Service
	sandbox *Sandbox // nil unless sandbox simulation is enabled
	log     *slog.Logger
}

// NewHandler wires payment routes. Pass a non-nil sandbox to expose the
// simulate endpoint (never in production; enforced by config).
func NewHandler(svc *Service, sandbox *Sandbox, log *slog.Logger) *Handler {
	return &Handler{svc: svc, sandbox: sandbox, log: log}
}

// Routes mounts /payments (authenticated).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/deposits", httpx.Handle(h.log, h.initiate))
	r.Get("/deposits", httpx.Handle(h.log, h.list))
	r.Get("/deposits/{depositId}", httpx.Handle(h.log, h.get))
	if h.sandbox != nil {
		r.Post("/sandbox/deposits/{depositId}/simulate", httpx.Handle(h.log, h.simulate))
	}
}

// WebhookRoutes mounts /webhooks (unauthenticated: trust comes from the
// provider's signature, verified per provider).
func (h *Handler) WebhookRoutes(r chi.Router) {
	r.Post("/payments/{provider}", httpx.Handle(h.log, h.webhook))
}

func (h *Handler) initiate(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	var req DepositRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	d, replayed, err := h.svc.InitiateDeposit(r.Context(), p.UserID, p.Email, req, r.Header.Get(idempotency.Header))
	if err != nil {
		return err
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
		w.Header().Set(idempotency.ReplayedHeader, "true")
	}
	httpx.OK(w, status, "Deposit initiated", d)
	return nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	res, err := h.svc.List(r.Context(), p.UserID, httpx.ParsePage(r))
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Deposits retrieved", res)
	return nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	id, err := httpx.UUIDParam(r, "depositId")
	if err != nil {
		return err
	}
	d, err := h.svc.Get(r.Context(), p.UserID, id)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Deposit retrieved", d)
	return nil
}

func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, httpx.MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return apperr.New(http.StatusRequestEntityTooLarge, apperr.CodePayloadTooLarge, "Webhook payload too large.")
		}
		return apperr.BadRequest(apperr.CodeBadRequest, "Could not read webhook body.")
	}
	if err := h.svc.HandleWebhook(r.Context(), chi.URLParam(r, "provider"), r.Header, body); err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Webhook processed", nil)
	return nil
}

type simulateRequest struct {
	Outcome string `json:"outcome" validate:"required,oneof=success failure"`
}

// simulate plays the payment provider in development: it builds a correctly
// signed sandbox webhook for the caller's own deposit and runs it through the
// exact same verification and settlement path as a real webhook.
func (h *Handler) simulate(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	id, err := httpx.UUIDParam(r, "depositId")
	if err != nil {
		return err
	}
	var req simulateRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	d, err := h.svc.Get(r.Context(), p.UserID, id)
	if err != nil {
		return err
	}
	if d.Provider != SandboxName {
		return apperr.BadRequest(apperr.CodeBadRequest, "Only sandbox deposits can be simulated.")
	}

	payload := SandboxPayload{ID: "evt_" + uuid.Must(uuid.NewV7()).String(), Type: "payment.succeeded"}
	payload.Data.Reference, payload.Data.Amount, payload.Data.Currency = d.Reference, d.Amount.Int64(), d.Currency
	if req.Outcome == "failure" {
		payload.Type = "payment.failed"
		payload.Data.FailureReason = "card_declined (simulated)"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := h.svc.HandleWebhook(r.Context(), SandboxName, h.sandbox.SignedRequestHeaders(body), body); err != nil {
		return err
	}

	updated, err := h.svc.Get(r.Context(), p.UserID, id)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Sandbox payment simulated", updated)
	return nil
}
