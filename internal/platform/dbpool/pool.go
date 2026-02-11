package dbpool

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/todo-1m/project/internal/platform/env"
)

const (
	defaultMinConns        = 2
	defaultMaxConns        = 20
	defaultMaxConnLifetime = 30 * time.Minute
	defaultMaxConnIdleTime = 5 * time.Minute
	defaultHealthCheck     = 30 * time.Second
)

func New(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}

	minConns := env.Int("DB_MIN_CONNS", defaultMinConns)
	maxConns := env.Int("DB_MAX_CONNS", defaultMaxConns)
	if minConns < 0 {
		minConns = defaultMinConns
	}
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}
	if minConns > maxConns {
		minConns = maxConns
	}

	cfg.MinConns = int32(minConns)
	cfg.MaxConns = int32(maxConns)
	cfg.MaxConnLifetime = env.Duration("DB_MAX_CONN_LIFETIME", defaultMaxConnLifetime)
	cfg.MaxConnIdleTime = env.Duration("DB_MAX_CONN_IDLE_TIME", defaultMaxConnIdleTime)
	cfg.HealthCheckPeriod = env.Duration("DB_HEALTH_CHECK_PERIOD", defaultHealthCheck)

	return pgxpool.NewWithConfig(ctx, cfg)
}
