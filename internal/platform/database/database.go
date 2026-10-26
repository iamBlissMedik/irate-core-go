// Package database owns the PostgreSQL connection pool and the transaction
// helper every service uses.
package database

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
)

// PoolConfig tunes the connection pool.
type PoolConfig struct {
	URL              string
	MaxConns         int32
	MinConns         int32
	StatementTimeout time.Duration
}

// NewPool creates and verifies a pgx connection pool.
//
// statement_timeout bounds any single query, and
// idle_in_transaction_session_timeout kills sessions that hold locks while
// doing nothing (for example, a crashed request that left a transaction open).
// Both protect the database from one bad request taking it down.
func NewPool(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnLifetime = time.Hour
	pc.MaxConnLifetimeJitter = 5 * time.Minute
	pc.MaxConnIdleTime = 15 * time.Minute
	pc.HealthCheckPeriod = 30 * time.Second
	pc.ConnConfig.RuntimeParams["application_name"] = "irate-core-go"
	pc.ConnConfig.RuntimeParams["timezone"] = "UTC"
	if cfg.StatementTimeout > 0 {
		pc.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
		pc.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = strconv.FormatInt((3 * cfg.StatementTimeout).Milliseconds(), 10)
	}
	// A request queued behind a long-held row lock (a very hot wallet) gives
	// up after this long and the client gets a retryable 503, instead of the
	// request holding a pooled connection until its own timeout.
	pc.ConnConfig.RuntimeParams["lock_timeout"] = "5000"

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Store bundles the pool with the sqlc queries. Its embedded *db.Queries runs
// on the pool (autocommit) for reads; use InTx for anything that writes more
// than one row or depends on what it read.
type Store struct {
	*db.Queries
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Queries: db.New(pool), pool: pool}
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

const maxTxAttempts = 3

// InTx runs fn in a READ COMMITTED transaction. It commits if fn returns nil
// and rolls back otherwise.
//
// If Postgres aborts the transaction with a serialization failure or a
// deadlock (both safe to retry: nothing was committed), the whole of fn is run
// again, up to maxTxAttempts times with jittered backoff. fn must therefore
// have no side effects outside the transaction.
func (s *Store) InTx(ctx context.Context, fn func(q *db.Queries) error) error {
	var err error
	for attempt := 1; attempt <= maxTxAttempts; attempt++ {
		err = s.runTx(ctx, fn)
		if hasCode(err, codeLockNotAvailable, "") {
			return apperr.Unavailable("This account is busy right now. Please retry in a moment.").WithCause(err)
		}
		if err == nil || !isRetryable(err) || attempt == maxTxAttempts {
			return err
		}
		backoff := time.Duration(attempt*attempt)*10*time.Millisecond + time.Duration(rand.IntN(10))*time.Millisecond //nolint:gosec // retry jitter, not security-sensitive
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
	return err
}

func (s *Store) runTx(ctx context.Context, fn func(q *db.Queries) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			// Roll back even if the request context was cancelled, so the
			// connection goes back to the pool clean.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err = fn(db.New(tx)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// Postgres SQLSTATE codes we act on.
const (
	codeUniqueViolation      = "23505"
	codeCheckViolation       = "23514"
	codeSerializationFailure = "40001"
	codeDeadlockDetected     = "40P01"
	codeNumericOverflow      = "22003"
	codeLockNotAvailable     = "55P03"
)

func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == codeSerializationFailure || pgErr.Code == codeDeadlockDetected
	}
	return false
}

// IsNoRows reports whether err means "the query matched nothing".
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// IsUniqueViolation reports whether err is a unique violation on constraint
// (any constraint when constraint is "").
func IsUniqueViolation(err error, constraint string) bool {
	return hasCode(err, codeUniqueViolation, constraint)
}

// IsCheckViolation reports whether err is a CHECK violation on constraint.
func IsCheckViolation(err error, constraint string) bool {
	return hasCode(err, codeCheckViolation, constraint)
}

// IsNumericOverflow reports whether a BIGINT (or other numeric) overflowed.
func IsNumericOverflow(err error) bool { return hasCode(err, codeNumericOverflow, "") }

func hasCode(err error, code, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}
