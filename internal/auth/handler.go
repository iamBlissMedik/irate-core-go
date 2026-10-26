package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/iamBlissMedik/irate-core-go/internal/config"
	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

type Handler struct {
	svc       *Service
	authn     *Authenticator
	cookie    config.AuthConfig
	rateLimit func(http.Handler) http.Handler
	log       *slog.Logger
}

// NewHandler wires the auth routes. rateLimit is applied to the credential
// endpoints (register, login, refresh); nil disables it.
func NewHandler(svc *Service, authn *Authenticator, cfg config.AuthConfig, rateLimit func(http.Handler) http.Handler, log *slog.Logger) *Handler {
	if rateLimit == nil {
		rateLimit = func(next http.Handler) http.Handler { return next }
	}
	return &Handler{svc: svc, authn: authn, cookie: cfg, rateLimit: rateLimit, log: log}
}

// Routes mounts /auth.
func (h *Handler) Routes(r chi.Router) {
	r.With(h.rateLimit).Post("/register", httpx.Handle(h.log, h.register))
	r.With(h.rateLimit).Post("/login", httpx.Handle(h.log, h.login))
	r.With(h.rateLimit).Post("/refresh", httpx.Handle(h.log, h.refresh))
	r.With(h.authn.Optional).Post("/logout", httpx.Handle(h.log, h.logout))
	r.With(h.authn.Required).Post("/logout-all", httpx.Handle(h.log, h.logoutAll))
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) error {
	var req RegisterRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	res, err := h.svc.Register(r.Context(), req)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusCreated, "User registered successfully", res)
	return nil
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) error {
	var req LoginRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	session, err := h.svc.Login(r.Context(), req)
	if err != nil {
		return err
	}
	h.setRefreshCookie(w, session.RefreshToken, session.RefreshExpiresAt)
	httpx.OK(w, http.StatusOK, "Login successful", session)
	return nil
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) error {
	session, err := h.svc.Refresh(r.Context(), h.refreshCookie(r))
	if err != nil {
		// Only a definitively invalid token clears the cookie; a transient
		// failure (database blip) must not sign the user out.
		if errors.Is(err, ErrRefreshInvalid) {
			h.clearRefreshCookie(w)
		}
		return err
	}
	h.setRefreshCookie(w, session.RefreshToken, session.RefreshExpiresAt)
	httpx.OK(w, http.StatusOK, "Access token refreshed", session)
	return nil
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) error {
	var principal *reqctx.Principal
	if p, ok := reqctx.PrincipalFrom(r.Context()); ok {
		principal = &p
	}
	if err := h.svc.Logout(r.Context(), principal, h.refreshCookie(r)); err != nil {
		return err
	}
	h.clearRefreshCookie(w)
	httpx.OK(w, http.StatusOK, "Logged out successfully", nil)
	return nil
}

func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.Principal(r)
	if err != nil {
		return err
	}
	if err := h.svc.LogoutAll(r.Context(), p); err != nil {
		return err
	}
	h.clearRefreshCookie(w)
	httpx.OK(w, http.StatusOK, "Logged out of all sessions", nil)
	return nil
}

func (h *Handler) refreshCookie(r *http.Request) string {
	c, err := r.Cookie(h.cookie.RefreshCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func (h *Handler) setRefreshCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookie.RefreshCookieName,
		Value:    token,
		Path:     h.cookie.RefreshCookiePath, // only ever sent to /api/v1/auth/*
		Domain:   h.cookie.CookieDomain,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,                    // unreadable from JavaScript (XSS)
		Secure:   h.cookie.CookieSecure,   // HTTPS only in production
		SameSite: http.SameSiteStrictMode, // never sent on cross-site requests (CSRF)
	})
}

func (h *Handler) clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookie.RefreshCookieName,
		Value:    "",
		Path:     h.cookie.RefreshCookiePath,
		Domain:   h.cookie.CookieDomain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookie.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}
