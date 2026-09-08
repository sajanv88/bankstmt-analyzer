// Package db holds the sqlc-generated query layer plus the pgx connection
// pool that backs it.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig tunes the pgx pool. Zero values fall back to the defaults
// below, so callers can pass an empty struct.
type PoolConfig struct {
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
	ConnectTimeout    time.Duration
}

// NewPool parses dsn, applies cfg, connects, and verifies the connection
// with a ping so that a bad DATABASE_URL fails startup rather than the
// first request.
func NewPool(ctx context.Context, dsn string, cfg PoolConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// The DSN carries credentials, so report the failure without it.
		return nil, fmt.Errorf("db: parse DATABASE_URL: %w", redactDSN(err, dsn))
	}
	applyDefaults(poolCfg, cfg)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", redactDSN(err, dsn))
	}

	pingCtx, cancel := context.WithTimeout(ctx, poolCfg.ConnConfig.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", redactDSN(err, dsn))
	}
	return pool, nil
}

func applyDefaults(poolCfg *pgxpool.Config, cfg PoolConfig) {
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.HealthCheckPeriod > 0 {
		poolCfg.HealthCheckPeriod = cfg.HealthCheckPeriod
	}
	if cfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}
	if poolCfg.ConnConfig.ConnectTimeout <= 0 {
		poolCfg.ConnConfig.ConnectTimeout = 10 * time.Second
	}
}
