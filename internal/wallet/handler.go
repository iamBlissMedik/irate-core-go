package wallet

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// Routes mounts /wallets (authenticated).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/", httpx.Handle(h.log, h.create))
	r.Get("/", httpx.Handle(h.log, h.list))
	r.Get("/{walletId}/balance", httpx.Handle(h.log, h.balance))
}

type createRequest struct {
	Currency string `json:"currency" validate:"omitempty,len=3,alpha"`
}

func (r *createRequest) Normalize() { r.Currency = strings.ToUpper(strings.TrimSpace(r.Currency)) }

func (h *Handler) create(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	var req createRequest
	if err := httpx.DecodeOptional(w, r, &req); err != nil {
		return err
	}
	wallet, err := h.svc.Create(r.Context(), p.UserID, req.Currency)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusCreated, "Wallet created successfully", wallet)
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
	httpx.OK(w, http.StatusOK, "Wallets retrieved", res)
	return nil
}

func (h *Handler) balance(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	walletID, err := httpx.UUIDParam(r, "walletId")
	if err != nil {
		return err
	}
	bal, err := h.svc.Balance(r.Context(), p.UserID, walletID)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Wallet balance retrieved", bal)
	return nil
}
