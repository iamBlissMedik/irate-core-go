// Package migrate applies the embedded goose migrations.
package migrate

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/iamBlissMedik/irate-core-go/migrations"
)

// Command runs a goose command ("up", "down", "status", "version", "reset",
// "redo", "up-by-one") against the database behind pool.
//
// goose takes a Postgres advisory lock while migrating, so several instances
// starting at once (AUTO_MIGRATE=true) cannot apply migrations concurrently.
func Command(ctx context.Context, pool *pgxpool.Pool, command string, log *slog.Logger) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }() // closes the wrapper only; the pool stays open

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS,
		goose.WithSessionLocker(mustLocker()),
	)
	if err != nil {
		return fmt.Errorf("init migrations: %w", err)
	}

	switch command {
	case "up":
		results, err := provider.Up(ctx)
		logResults(log, results)
		return err
	case "up-by-one":
		res, err := provider.UpByOne(ctx)
		logResults(log, []*goose.MigrationResult{res})
		return err
	case "down":
		res, err := provider.Down(ctx)
		logResults(log, []*goose.MigrationResult{res})
		return err
	case "reset":
		results, err := provider.DownTo(ctx, 0)
		logResults(log, results)
		return err
	case "redo":
		if res, err := provider.Down(ctx); err != nil {
			return err
		} else {
			logResults(log, []*goose.MigrationResult{res})
		}
		res, err := provider.UpByOne(ctx)
		logResults(log, []*goose.MigrationResult{res})
		return err
	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range statuses {
			log.Info("migration", "version", s.Source.Version, "file", s.Source.Path, "state", s.State, "applied_at", s.AppliedAt)
		}
		return nil
	case "version":
		v, err := provider.GetDBVersion(ctx)
		if err != nil {
			return err
		}
		log.Info("database schema version", "version", v)
		return nil
	default:
		return fmt.Errorf("unknown migrate command %q (use up, up-by-one, down, redo, reset, status, version)", command)
	}
}

func logResults(log *slog.Logger, results []*goose.MigrationResult) {
	for _, r := range results {
		if r == nil || r.Source == nil {
			continue
		}
		log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "direction", r.Direction, "duration", r.Duration)
	}
	if len(results) == 0 {
		log.Info("no migrations to apply")
	}
}
