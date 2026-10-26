// Package app is the composition root: it builds every dependency explicitly
// (no DI framework, no globals), wires the HTTP router and owns the process
// lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/iamBlissMedik/irate-core-go/internal/config"
	"github.com/iamBlissMedik/irate-core-go/internal/jobs"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/metrics"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/migrate"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/redisx"
)

// Version is stamped at build time:
// go build -ldflags "-X github.com/iamBlissMedik/irate-core-go/internal/app.Version=v1.2.3"
var Version = "dev"

type App struct {
	cfg     config.Config
	log     *slog.Logger
	pool    *pgxpool.Pool
	rdb     *redis.Client
	metrics *metrics.Metrics
	handler http.Handler
	janitor *jobs.Janitor
	ready   atomic.Bool
}

// New connects to Postgres and Redis, optionally migrates, and builds the
// router. Call Close when done.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	pool, err := database.NewPool(ctx, database.PoolConfig{
		URL: cfg.DatabaseURL, MaxConns: cfg.DBMaxConns, MinConns: cfg.DBMinConns, StatementTimeout: cfg.DBStatementTimeout,
	})
	if err != nil {
		return nil, err
	}
	if cfg.AutoMigrate {
		if err := migrate.Command(ctx, pool, "up", log); err != nil {
			pool.Close()
			return nil, fmt.Errorf("auto-migrate: %w", err)
		}
	}
	rdb, err := redisx.New(ctx, cfg.RedisURL)
	if err != nil {
		pool.Close()
		return nil, err
	}

	a := &App{
		cfg:     cfg,
		log:     log,
		pool:    pool,
		rdb:     rdb,
		metrics: metrics.New(),
		janitor: jobs.NewJanitor(pool, time.Hour, cfg.IdempotencyTTL, log),
	}
	a.handler = a.routes()
	a.ready.Store(true)
	return a, nil
}

// Handler exposes the router (used by integration tests via httptest).
func (a *App) Handler() http.Handler { return a.handler }

// Pool and Redis expose the clients to integration tests.
func (a *App) Pool() *pgxpool.Pool    { return a.pool }
func (a *App) Redis() *redis.Client   { return a.rdb }
func (a *App) Janitor() *jobs.Janitor { return a.janitor }

// Run serves until ctx is cancelled (SIGINT/SIGTERM), then shuts down
// gracefully: readiness flips to 503 so load balancers stop routing, in-flight
// requests get ShutdownTimeout to finish, then background work stops.
func (a *App) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", a.cfg.HTTPPort),
		Handler:           a.handler,
		ReadHeaderTimeout: 5 * time.Second, // Slowloris protection
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      a.cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}

	servers := []*http.Server{srv}
	if a.cfg.MetricsPort > 0 {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", a.metrics.Handler())
		servers = append(servers, &http.Server{
			Addr: fmt.Sprintf(":%d", a.cfg.MetricsPort), Handler: mux,
			ReadHeaderTimeout: 5 * time.Second, ErrorLog: srv.ErrorLog,
		})
	}

	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			a.log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("server %s: %w", s.Addr, err)
			}
		}()
	}

	jobsCtx, stopJobs := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Go(func() { a.janitor.Run(jobsCtx) })

	a.log.Info("irate-core-go started", "version", Version, "port", a.cfg.HTTPPort)

	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutdown signal received; draining")
	case runErr = <-errCh:
		a.log.Error("server failed; shutting down", "err", runErr)
	}

	a.ready.Store(false)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.ShutdownTimeout)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			a.log.Error("graceful shutdown incomplete", "addr", s.Addr, "err", err)
			runErr = errors.Join(runErr, err)
		}
	}
	stopJobs()
	wg.Wait()
	a.log.Info("shutdown complete")
	return runErr
}

// Close releases the database and Redis clients.
func (a *App) Close() {
	if a.rdb != nil {
		_ = a.rdb.Close()
	}
	if a.pool != nil {
		a.pool.Close()
	}
}
