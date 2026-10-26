package kyc

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// Routes mounts the customer routes under /kyc (authenticated).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/submit", httpx.Handle(h.log, h.submit))
	r.Get("/status", httpx.Handle(h.log, h.status))
}

// AdminRoutes mounts the review routes under /admin (admin-only).
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Get("/kyc", httpx.Handle(h.log, h.list))
	r.Patch("/kyc/{kycId}/review", httpx.Handle(h.log, h.review))
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	var req SubmitRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	rec, err := h.svc.Submit(r.Context(), p.UserID, req)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusCreated, "KYC submitted successfully", rec)
	return nil
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	rec, err := h.svc.Mine(r.Context(), p.UserID)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "KYC retrieved", rec)
	return nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	res, err := h.svc.List(r.Context(), r.URL.Query().Get("status"), httpx.ParsePage(r))
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "KYC submissions retrieved", res)
	return nil
}

func (h *Handler) review(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	kycID, err := httpx.UUIDParam(r, "kycId")
	if err != nil {
		return err
	}
	var req ReviewRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	rec, err := h.svc.Review(r.Context(), p.UserID, kycID, req)
	if err != nil {
		return err
	}
	msg := "KYC approved"
	if req.Action == "REJECT" {
		msg = "KYC rejected"
	}
	httpx.OK(w, http.StatusOK, msg, rec)
	return nil
}
