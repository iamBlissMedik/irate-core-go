package user

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

// Routes mounts /users (authenticated). "/me/overview" and "/me" are
// registered before "/{userId}"; chi prefers static segments anyway.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/me/overview", httpx.Handle(h.log, h.overview))
	r.Get("/me", httpx.Handle(h.log, h.me))
	r.Get("/{userId}", httpx.Handle(h.log, h.byID))
}

// AdminRoutes mounts the directory and the admin's own profile under /admin.
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Get("/users", httpx.Handle(h.log, h.list))
	r.Get("/me", httpx.Handle(h.log, h.me))
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	ov, err := h.svc.Overview(r.Context(), p.UserID)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Overview fetched successfully", ov)
	return nil
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	prof, err := h.svc.Profile(r.Context(), p.UserID)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "User fetched successfully", prof)
	return nil
}

func (h *Handler) byID(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	id, err := httpx.UUIDParam(r, "userId")
	if err != nil {
		return err
	}
	prof, err := h.svc.ProfileFor(r.Context(), p, id)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "User fetched successfully", prof)
	return nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	res, err := h.svc.List(r.Context(), r.URL.Query().Get("search"), httpx.ParsePage(r))
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, "Users retrieved successfully", res)
	return nil
}
