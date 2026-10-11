// Package storage owns the application database: the connection pool, the
// schema migrations, and the repositories built on top of them.
//
// This is the mediad database. Temporal keeps its own two (temporal and
// temporal_visibility) and manages their schema itself; nothing here touches
// them.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
	"go.uber.org/fx"

	"github.com/alex/mediad/internal/config"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsRoot is the directory inside migrationsFS that holds the .sql
// files.
//
// The embed directive keeps the directory prefix, so the files land at
// "migrations/00001_init.sql" rather than at the root. goose reads the fs.FS
// it is given as already rooted at the migration directory — there is no
// "migrations dir" option on the provider — so the prefix has to be stripped
// or it finds nothing and reports "no migrations found".
const migrationsRoot = "migrations"

// Module wires the connection pool into the fx graph and applies migrations
// before anything that depends on the schema is constructed.
var Module = fx.Module("storage",
	fx.Provide(NewPool),
	fx.Invoke(func(*pgxpool.Pool) {}),
)

// NewPool opens the pool, verifies connectivity, applies migrations and
// registers the pool's lifetime with fx.
//
// Migrations run here rather than in a dedicated command so that every entry
// point — api, worker, cli — starts against a current schema. They are
// idempotent and guarded by an advisory lock, so several processes starting at
// once is safe.
func NewPool(lc fx.Lifecycle, cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Postgres.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	poolCfg.MaxConns = cfg.Postgres.MaxConns

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}

	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if err := Migrate(context.Background(), pool); err != nil {
		pool.Close()
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			pool.Close()
			return nil
		},
	})

	return pool, nil
}

// Migrate applies every pending migration.
//
// goose takes a *sql.DB, so the pgx pool is wrapped rather than duplicated:
// stdlib.OpenDBFromPool shares the pool's connections instead of opening a
// second set.
//
// The session locker is what makes concurrent startup safe. Without it two
// processes that start together both read "no migrations applied" and both try
// to create the same tables; the loser fails on a duplicate-object error and
// refuses to start. The lock serialises them, so the second one finds nothing
// pending and no-ops.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	return MigrateDB(ctx, db)
}

// MigrateDB is Migrate against an existing *sql.DB, which is what tests use.
func MigrateDB(ctx context.Context, db *sql.DB) error {
	fsys, err := fs.Sub(migrationsFS, migrationsRoot)
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("build migration lock: %w", err)
	}

	provider, err := goose.NewProvider(
		database.DialectPostgres,
		db,
		fsys,
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		return fmt.Errorf("build migration provider: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	_ = results // the caller logs these when it cares

	return nil
}
