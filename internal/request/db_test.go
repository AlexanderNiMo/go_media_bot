package request

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/storage"
)

// A throwaway database for the tests that need one, built the same way the
// storage tests build theirs.

const testDSNEnv = "MEDIAD_TEST_DSN"

func newTestStore(t *testing.T) (*storage.Store, *pgxpool.Pool) {
	t.Helper()

	adminDSN := os.Getenv(testDSNEnv)
	if adminDSN == "" {
		t.Skipf("%s is not set; run `make up` and export it to run database tests", testDSNEnv)
	}

	ctx := context.Background()
	name := fmt.Sprintf("mediad_req_%d_%s", time.Now().UnixNano(), sanitizeName(t.Name()))

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	defer admin.Close(ctx)

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create the test database: %v", err)
	}

	// The database goes on the parsed config rather than into a rewritten DSN:
	// pgx's ConnConfig.ConnString() returns the string it was built from, so
	// editing cfg.Database and rebuilding the string silently yields the
	// original and every test shares one database.
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		t.Fatalf("parse the dsn: %v", err)
	}
	cfg.ConnConfig.Database = name

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropTestDatabase(adminDSN, name)
	})

	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return storage.NewStore(pool, slog.New(slog.DiscardHandler)), pool
}

func dropTestDatabase(adminDSN, name string) {
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return
	}
	defer conn.Close(ctx)

	_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

// insertMedia adds a media row and returns its id.
func insertMedia(t *testing.T, pool *pgxpool.Pool, mediaType string, tmdbID int64, season *int16, title string) int64 {
	t.Helper()

	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO media (media_type, tmdb_id, season, title) VALUES ($1, $2, $3, $4) RETURNING id`,
		mediaType, tmdbID, season, title).Scan(&id)
	if err != nil {
		t.Fatalf("insert media: %v", err)
	}
	return id
}

// movieSpec is the spec the fixtures request.
func movieSpec() domain.MediaSpec {
	released := time.Date(1999, time.March, 31, 0, 0, 0, 0, time.UTC)
	return domain.MediaSpec{
		Ref:        domain.MovieRef(603),
		Title:      "The Matrix",
		TitleRU:    "Матрица",
		Year:       1999,
		RuntimeMin: 136,
		ReleasedAt: &released,
	}
}

func sanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '_')
		}
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return string(out)
}
