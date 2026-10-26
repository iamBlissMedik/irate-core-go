// Package redisx creates the Redis client.
//
// Redis holds only short-lived, reconstructible state: rate-limit counters,
// login-failure counters and access-token revocations. Money, sessions and
// idempotency records live in Postgres, so losing Redis never loses or
// duplicates a transaction.
package redisx

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// New parses url (redis://user:pass@host:port/db, or rediss:// for TLS) and
// verifies the connection.
func New(ctx context.Context, url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	opts.DialTimeout = 3 * time.Second
	opts.ReadTimeout = 2 * time.Second
	opts.WriteTimeout = 2 * time.Second
	opts.PoolTimeout = 3 * time.Second

	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}
