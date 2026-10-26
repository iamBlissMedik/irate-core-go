package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"

	"github.com/iamBlissMedik/irate-core-go/internal/admin"
	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/auth"
	"github.com/iamBlissMedik/irate-core-go/internal/config"
	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/idempotency"
	"github.com/iamBlissMedik/irate-core-go/internal/kyc"
	"github.com/iamBlissMedik/irate-core-go/internal/payment"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/metrics"
	"github.com/iamBlissMedik/irate-core-go/internal/ratelimit"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
	"github.com/iamBlissMedik/irate-core-go/internal/statement"
	"github.com/iamBlissMedik/irate-core-go/internal/transfer"
	"github.com/iamBlissMedik/irate-core-go/internal/user"
	"github.com/iamBlissMedik/irate-core-go/internal/wallet"
)

func (a *App) routes() http.Handler {
	cfg, log, m := a.cfg, a.log, a.metrics

	// ---- services -----------------------------------------------------
	store := database.NewStore(a.pool)
	wallets := wallet.NewService(store, cfg.Money)
	statements := statement.NewService(store, wallets)
	tokens := auth.NewTokenManager(cfg.Auth.AccessSecret, cfg.Auth.PreviousAccessSecret, cfg.Auth.Issuer, cfg.Auth.Audience, cfg.Auth.AccessTTL)
	revocations := auth.NewRevocations(a.rdb)
	authn := auth.NewAuthenticator(tokens, revocations, log)
	authSvc := auth.NewService(auth.Deps{
		Store:           store,
		Wallets:         wallets,
		Tokens:          tokens,
		Hasher:          auth.NewHasher(cfg.Auth.MaxConcurrentHashes),
		Lockout:         auth.NewLockout(a.rdb, cfg.Auth.LoginMaxAttempts, cfg.Auth.LoginLockout),
		Revocations:     revocations,
		Config:          cfg.Auth,
		DefaultCurrency: cfg.Money.DefaultCurrency,
		Metrics:         m,
		Log:             log,
	})
	kycSvc := kyc.NewService(store, cfg.Money.BusinessLocation)
	userSvc := user.NewService(store, kycSvc, statements, cfg.Money.DefaultCurrency)
	transferSvc := transfer.NewService(store, cfg.Money, m)
	adminSvc := admin.NewService(store, cfg.Money, m)
	paymentSvc, sandbox := newPaymentService(cfg, store, m, log)
	limiter := ratelimit.New(a.rdb, cfg.RateLimitEnabled, m, log)

	// ---- handlers -----------------------------------------------------
	var simulator *payment.Sandbox
	if cfg.Payments.SandboxSimulationEnabled && sandbox != nil {
		simulator = sandbox
	}
	authH := auth.NewHandler(authSvc, authn, cfg.Auth, limiter.Middleware(ratelimit.PolicyAuth, ratelimit.ByIP), log)
	userH := user.NewHandler(userSvc, log)
	walletH := wallet.NewHandler(wallets, log)
	transferH := transfer.NewHandler(transferSvc, statements, wallets, transfer.Middleware{
		Transfer: limiter.Middleware(ratelimit.PolicyTransfer, ratelimit.ByUser),
		Enquiry:  limiter.Middleware(ratelimit.PolicyEnquiry, ratelimit.ByUser),
	}, log)
	kycH := kyc.NewHandler(kycSvc, log)
	adminH := admin.NewHandler(adminSvc, log)
	paymentH := payment.NewHandler(paymentSvc, simulator, log)

	// ---- router -------------------------------------------------------
	r := chi.NewRouter()
	// Order matters: request id first (everything logs it), then client info,
	// the access log (so it records the final status, including recovered
	// panics), then security headers, CORS (answers preflights before auth)
	// and finally the per-request deadline.
	r.Use(
		httpx.RequestID,
		httpx.ClientInfo(cfg.TrustedProxyHops),
		httpx.AccessLog(log, m),
		httpx.Recoverer(log),
		httpx.SecureHeaders(cfg.IsProduction()),
		cors.Handler(cors.Options{
			AllowedOrigins:   cfg.CORSAllowedOrigins,
			AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions},
			AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", idempotency.Header, httpx.RequestIDHeader},
			ExposedHeaders:   []string{httpx.RequestIDHeader, idempotency.ReplayedHeader, "Retry-After", "RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset"},
			AllowCredentials: true,
			MaxAge:           300,
		}),
		httpx.Timeout(cfg.RequestTimeout),
	)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, log, apperr.NotFound(apperr.CodeNotFound, "Route not found: "+r.Method+" "+r.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, log, apperr.New(http.StatusMethodNotAllowed, apperr.CodeMethodNotAllowed, "Method "+r.Method+" is not allowed on "+r.URL.Path))
	})

	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.OK(w, http.StatusOK, "Welcome to iRate Core (Go)", map[string]string{"version": Version})
	})
	r.Get("/health", a.liveness)
	r.Get("/healthz", a.liveness)
	r.Get("/readyz", a.readiness)

	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/auth", authH.Routes)
		r.Route("/webhooks", paymentH.WebhookRoutes)

		r.Group(func(r chi.Router) {
			r.Use(authn.Required, limiter.Middleware(ratelimit.PolicyUser, ratelimit.ByUser))
			r.Route("/users", userH.Routes)
			r.Route("/wallets", walletH.Routes)
			r.Route("/transactions", transferH.Routes)
			r.Route("/kyc", kycH.Routes)
			r.Route("/payments", paymentH.Routes)
			r.Route("/admin", func(r chi.Router) {
				r.Use(auth.RequireRole(reqctx.RoleAdmin, log))
				adminH.Routes(r)
				userH.AdminRoutes(r)
				kycH.AdminRoutes(r)
			})
		})
	})
	return r
}

// liveness: the process is up. Never checks dependencies; restarting the pod
// would not fix a database outage.
func (a *App) liveness(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok", "timestamp": time.Now().UTC()})
}

// readiness: this instance can serve traffic (dependencies reachable, not
// draining). Load balancers route only to ready instances.
func (a *App) readiness(w http.ResponseWriter, r *http.Request) {
	if !a.ready.Load() {
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]any{"status": "draining"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	checks, status := map[string]string{"database": "ok", "redis": "ok"}, http.StatusOK
	if err := a.pool.Ping(ctx); err != nil {
		checks["database"], status = "unavailable", http.StatusServiceUnavailable
	}
	if err := a.rdb.Ping(ctx).Err(); err != nil {
		checks["redis"], status = "unavailable", http.StatusServiceUnavailable
	}
	label := "ready"
	if status != http.StatusOK {
		label = "degraded"
	}
	httpx.JSON(w, status, map[string]any{"status": label, "checks": checks})
}

// newPaymentService registers the active provider (used for new deposits) and
// keeps the sandbox available where allowed, so deposits started on it still
// settle. It returns the sandbox (nil when not registered) for the simulator.
func newPaymentService(cfg config.Config, store *database.Store, m *metrics.Metrics, log *slog.Logger) (*payment.Service, *payment.Sandbox) {
	var sandbox *payment.Sandbox
	if cfg.Payments.SandboxEnabled(cfg.IsProduction()) {
		sandbox = payment.NewSandbox(cfg.Payments.SandboxWebhookSecret, cfg.Payments.PublicBaseURL)
	}
	var (
		active payment.Provider
		others []payment.Provider
	)
	switch cfg.Payments.Provider {
	case payment.PaystackName:
		active = payment.NewPaystack(cfg.Payments.PaystackSecretKey, cfg.Payments.PaystackBaseURL, cfg.Payments.PaystackCallbackURL, nil)
		if sandbox != nil {
			others = append(others, sandbox)
		}
	default: // "sandbox" (config validation rejects anything else, and SandboxEnabled is true for it)
		active = sandbox
	}
	return payment.NewService(store, active, others, cfg.Money, m, log), sandbox
}
