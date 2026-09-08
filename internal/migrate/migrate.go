// Package migrate applies the embedded goose migrations to a database.
//
// It exists so that a single binary can bring an empty database fully up to
// date — application tables and taskQ's broker table alike — before either
// the API or the worker starts serving.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	migrations "github.com/sajanv88/bankstmt-analyzer/db/migrations"
)

// Up applies every pending migration and logs what it did.
//
// Migrations run under a Postgres advisory lock, so several replicas
// starting at once — or a Helm pre-upgrade Job racing a pod with
// MIGRATE_ON_START set — serialise instead of colliding. Whoever loses the
// race waits, then finds nothing left to apply.
func Up(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	provider, db, err := newProvider(pool)
	if err != nil {
		return err
	}
	// Closing this *sql.DB releases its handles without touching the
	// caller-owned pgxpool, which OpenDBFromPool deliberately leaves open.
	defer func() { _ = db.Close() }()

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrate: apply migrations: %w", err)
	}
	if len(results) == 0 {
		logger.InfoContext(ctx, "database schema already up to date")
		return nil
	}
	for _, r := range results {
		logger.InfoContext(ctx, "migration applied",
			// Not "version": the process logger already binds that key to
			// the build version, and two identical keys in one JSON object
			// is not something a log pipeline can be relied on to keep.
			"migration_version", r.Source.Version,
			"path", r.Source.Path,
			"duration", r.Duration,
		)
	}
	logger.InfoContext(ctx, "migrations complete", "applied", len(results))
	return nil
}

func newProvider(pool *pgxpool.Pool) (*goose.Provider, *sql.DB, error) {
	if pool == nil {
		return nil, nil, fmt.Errorf("migrate: nil database pool")
	}
	db := stdlib.OpenDBFromPool(pool)

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("migrate: build session locker: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("migrate: build provider: %w", err)
	}
	return provider, db, nil
}
