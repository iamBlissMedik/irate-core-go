// Package jobs runs background maintenance.
package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iamBlissMedik/irate-core-go/internal/db"
)

// janitorLockID is an arbitrary, fixed advisory-lock key ("irate" + 1).
const janitorLockID int64 = 0x69726174_00000001

// Janitor deletes expired idempotency keys and refresh tokens.
//
// Every API replica runs a Janitor, but each run first takes a Postgres
// advisory lock with pg_try_advisory_lock, so only one replica does the work
// at a time; the others skip that tick. No separate scheduler or leader
// election service is needed.
type Janitor struct {
	pool           *pgxpool.Pool
	interval       time.Duration
	idempotencyTTL time.Duration
	refreshGrace   time.Duration // keep expired tokens a while for reuse forensics
	log            *slog.Logger
}

func NewJanitor(pool *pgxpool.Pool, interval, idempotencyTTL time.Duration, log *slog.Logger) *Janitor {
	return &Janitor{pool: pool, interval: interval, idempotencyTTL: idempotencyTTL, refreshGrace: 7 * 24 * time.Hour, log: log}
}

// Run blocks until ctx is cancelled.
func (j *Janitor) Run(ctx context.Context) {
	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := j.RunOnce(ctx); err != nil && ctx.Err() == nil {
				j.log.WarnContext(ctx, "janitor run failed", "err", err)
			}
		}
	}
}

// RunOnce performs one cleanup pass if this instance wins the lock.
func (j *Janitor) RunOnce(ctx context.Context) error {
	// Session-level advisory locks belong to a connection, so lock, work and
	// unlock all happen on one dedicated connection.
	conn, err := j.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", janitorLockID).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return nil // another replica is cleaning up
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", janitorLockID)
	}()

	q := db.New(conn)
	now := time.Now()
	keys, err := q.DeleteIdempotencyKeysBefore(ctx, now.Add(-j.idempotencyTTL))
	if err != nil {
		return err
	}
	tokens, err := q.DeleteExpiredRefreshTokens(ctx, now.Add(-j.refreshGrace))
	if err != nil {
		return err
	}
	if keys > 0 || tokens > 0 {
		j.log.InfoContext(ctx, "janitor cleaned up", "idempotency_keys", keys, "refresh_tokens", tokens)
	}
	return nil
}
