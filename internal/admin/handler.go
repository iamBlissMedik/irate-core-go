package admin

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/idempotency"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// Routes mounts the admin-owned routes under /admin (admin-only).
func (h *Handler) Routes(r chi.Router) {
	r.Get("/dashboard/overview", httpx.Handle(h.log, h.overview))
	r.Get("/users/stats", httpx.Handle(h.log, h.usersStats))
	r.Get("/wallets/balance", httpx.Handle(h.log, h.walletsBalance))
	r.Post("/credit/{walletId}", httpx.Handle(h.log, h.credit))
	r.Patch("/wallets/{walletId}/status", httpx.Handle(h.log, h.walletStatus))
	r.Get("/reconciliation", httpx.Handle(h.log, h.reconciliation))
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) error {
	ov, err := h.svc.Overview(r.Context())
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Dashboard overview fetched successfully", ov)
	return nil
}

func (h *Handler) usersStats(w http.ResponseWriter, r *http.Request) error {
	card, err := h.svc.UsersStats(r.Context())
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "User stats fetched successfully", card)
	return nil
}

func (h *Handler) walletsBalance(w http.ResponseWriter, r *http.Request) error {
	card, err := h.svc.WalletsBalance(r.Context())
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Wallet balance fetched successfully", card)
	return nil
}

func (h *Handler) credit(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	walletID, err := httpx.UUIDParam(r, "walletId")
	if err != nil {
		return err
	}
	var req CreditRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	res, replayed, err := h.svc.Credit(r.Context(), p.UserID, walletID, req, r.Header.Get(idempotency.Header))
	if err != nil {
		return err
	}
	if replayed {
		w.Header().Set(idempotency.ReplayedHeader, "true")
	}
	httpx.OK(w, http.StatusOK, "Wallet credited successfully", res)
	return nil
}

func (h *Handler) walletStatus(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	walletID, err := httpx.UUIDParam(r, "walletId")
	if err != nil {
		return err
	}
	var req WalletStatusRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	res, err := h.svc.SetWalletStatus(r.Context(), p.UserID, walletID, req)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Wallet status updated", res)
	return nil
}

func (h *Handler) reconciliation(w http.ResponseWriter, r *http.Request) error {
	rec, err := h.svc.Reconcile(r.Context())
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Reconciliation completed", rec)
	return nil
}
