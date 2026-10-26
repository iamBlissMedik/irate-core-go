package transfer

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/idempotency"
	"github.com/iamBlissMedik/irate-core-go/internal/statement"
	"github.com/iamBlissMedik/irate-core-go/internal/wallet"
)

// Middleware lets the router attach route-specific policies (rate limits)
// without this package knowing how they are implemented.
type Middleware struct {
	Transfer func(http.Handler) http.Handler
	Enquiry  func(http.Handler) http.Handler
}

type Handler struct {
	svc        *Service
	statements *statement.Service
	wallets    *wallet.Service
	mw         Middleware
	log        *slog.Logger
}

func NewHandler(svc *Service, statements *statement.Service, wallets *wallet.Service, mw Middleware, log *slog.Logger) *Handler {
	if mw.Transfer == nil {
		mw.Transfer = passthrough
	}
	if mw.Enquiry == nil {
		mw.Enquiry = passthrough
	}
	return &Handler{svc: svc, statements: statements, wallets: wallets, mw: mw, log: log}
}

func passthrough(next http.Handler) http.Handler { return next }

// Routes mounts /transactions (authenticated).
func (h *Handler) Routes(r chi.Router) {
	r.With(h.mw.Enquiry).Get("/resolve/{accountNumber}", httpx.Handle(h.log, h.resolve))
	r.With(h.mw.Transfer).Post("/transfer", httpx.Handle(h.log, h.transfer))
	r.Get("/me", httpx.Handle(h.log, h.listMine))
	r.Get("/{walletId}/transactions", httpx.Handle(h.log, h.listWallet))
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) error {
	res, err := h.wallets.Resolve(r.Context(), chi.URLParam(r, "accountNumber"))
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Account resolved", res)
	return nil
}

func (h *Handler) transfer(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	var req Request
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	entry, replayed, err := h.svc.Transfer(r.Context(), p.UserID, req, r.Header.Get(idempotency.Header))
	if err != nil {
		return err
	}
	if replayed {
		w.Header().Set(idempotency.ReplayedHeader, "true")
	}
	httpx.OK(w, http.StatusOK, "Transfer successful", entry)
	return nil
}

func (h *Handler) listMine(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	page, err := h.statements.ForUser(r.Context(), p.UserID, httpx.ParsePage(r))
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Transaction history retrieved", page)
	return nil
}

func (h *Handler) listWallet(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	walletID, err := httpx.UUIDParam(r, "walletId")
	if err != nil {
		return err
	}
	page, err := h.statements.ForWallet(r.Context(), p.UserID, walletID, httpx.ParsePage(r))
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Transaction history retrieved", page)
	return nil
}
