// Command migrate applies database migrations.
//
//	go run ./cmd/migrate up        # apply all pending migrations
//	go run ./cmd/migrate status    # list migrations and their state
//	go run ./cmd/migrate down      # roll back the most recent migration
//
// It needs only DATABASE_URL, so it can run as a separate release step
// (e.g. a Kubernetes Job or a "migrate" container) before the API starts.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"log/slog"

	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/logging"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/migrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := logging.New(os.Stdout, slog.LevelInfo, "text", os.Getenv("APP_ENV"))
	pool, err := database.NewPool(ctx, database.PoolConfig{URL: url, MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()

	return migrate.Command(ctx, pool, command, log)
}
