package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDSNEnv names the connection string for an administrative connection to
// the test Postgres. Tests are skipped when it is unset, so `go test ./...`
// stays green on a machine with no database — start the stack with `make up`
// and export it to actually run these.
const testDSNEnv = "MEDIAD_TEST_DSN"

// newTestDB creates a throwaway database, applies the migrations to it, and
// returns a pool. The database is dropped when the test finishes.
//
// A fresh database per test rather than a truncate between them: the schema
// has enough partial indexes and constraints that clearing rows hides
// ordering bugs, and creating a database is cheap.
func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	adminDSN := os.Getenv(testDSNEnv)
	if adminDSN == "" {
		t.Skipf("%s is not set; run `make up` and export it to run database tests", testDSNEnv)
	}

	ctx := context.Background()
	name := fmt.Sprintf("mediad_test_%d_%s", time.Now().UnixNano(), sanitize(t.Name()))

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to admin database: %v", err)
	}
	defer admin.Close(ctx)

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	pool := openPool(t, adminDSN, name)

	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropDatabase(adminDSN, name)
	})

	return pool
}

// openPool returns a pool for the named database.
//
// The database is set on the parsed config rather than by rewriting the DSN,
// because pgx's ConnConfig.ConnString() returns the string it was built from —
// editing cfg.Database and calling ConnString() yields the ORIGINAL connection
// string, so every test would silently share one database. That failure looks
// like unrelated constraint violations in whichever test runs second.
func openPool(t *testing.T, dsn, database string) *pgxpool.Pool {
	t.Helper()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = database

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open pool for %s: %v", database, err)
	}
	return pool
}

func dropDatabase(adminDSN, name string) {
	ctx := context.Background()

	c, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return
	}
	defer c.Close(ctx)

	// WITH (FORCE) so a connection the test leaked does not wedge the drop.
	_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

func TestMigrateIsIdempotent(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	// newTestDB already migrated once; a second run must be a no-op rather
	// than an error about objects that already exist.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("third migrate: %v", err)
	}
}

// TestConcurrentMigrate is the case the session locker exists for: two
// processes starting at the same moment must not both try to create the
// schema, with the loser dying on a duplicate-object error.
func TestConcurrentMigrate(t *testing.T) {
	adminDSN := os.Getenv(testDSNEnv)
	if adminDSN == "" {
		t.Skipf("%s is not set", testDSNEnv)
	}

	ctx := context.Background()
	name := fmt.Sprintf("mediad_test_race_%d", time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx)

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { dropDatabase(adminDSN, name) })

	const racers = 4
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			pool := openPool(t, adminDSN, name)
			defer pool.Close()

			if err := Migrate(ctx, pool); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d of %d concurrent migrations failed, first: %v", len(errs), racers, errs[0])
	}
}

func TestSchemaObjectsExist(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	tables := []string{
		"users", "user_identities", "sessions", "invite_codes",
		"media", "requests", "request_watchers", "request_events",
		"notifications", "release_blacklist", "search_sessions",
	}
	for _, name := range tables {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			                 WHERE table_schema = 'public' AND table_name = $1)`, name).Scan(&exists)
		if err != nil {
			t.Fatalf("look up %s: %v", name, err)
		}
		if !exists {
			t.Errorf("table %s was not created", name)
		}
	}

	var viewExists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.views
		                 WHERE table_schema = 'public' AND table_name = 'request_view')`).Scan(&viewExists)
	if err != nil {
		t.Fatalf("look up view: %v", err)
	}
	if !viewExists {
		t.Error("view request_view was not created")
	}
}

// ---------------------------------------------------------------------------
// Deduplication
//
// These are the invariants the whole request flow leans on, so they are worth
// asserting against a real database rather than trusting the DDL by eye.
// ---------------------------------------------------------------------------

func TestMediaIdentityIsUnique(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	insert := func() error {
		_, err := pool.Exec(ctx,
			`INSERT INTO media (media_type, tmdb_id, title) VALUES ('movie', 603, 'The Matrix')`)
		return err
	}

	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("a second movie with the same tmdb_id was accepted")
	}
}

// TestMediaUniqueTreatsNullSeasonAsOneValue guards the COALESCE in the index.
// A plain UNIQUE treats NULLs as distinct, so without it a movie — whose
// season is NULL — could be inserted again and again.
func TestMediaUniqueTreatsNullSeasonAsOneValue(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		err := func() error {
			_, err := pool.Exec(ctx,
				`INSERT INTO media (media_type, tmdb_id, season, title)
				 VALUES ('tv', 1399, NULL, 'Game of Thrones')`)
			return err
		}()
		if i == 0 && err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if i == 1 && err == nil {
			t.Fatal("a second row with the same tmdb_id and NULL season was accepted")
		}
	}

	// Distinct seasons of one show are of course fine.
	for _, season := range []int{1, 2} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO media (media_type, tmdb_id, season, title)
			 VALUES ('tv', 1399, $1, 'Game of Thrones')`, season); err != nil {
			t.Fatalf("season %d: %v", season, err)
		}
	}
}

func TestOnlyOneActiveRequestPerMedia(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")

	insertRequest := func(state string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO requests (media_id, workflow_id, state, search_deadline)
			 VALUES ($1, 'movie-603', $2, now() + interval '2 years')`, mediaID, state)
		return err
	}

	if err := insertRequest("pending"); err != nil {
		t.Fatalf("first request: %v", err)
	}

	// Every active state must be refused a second row.
	for _, state := range []string{"pending", "searching", "adding", "downloading"} {
		if err := insertRequest(state); err == nil {
			t.Errorf("a second active request in state %q was accepted", state)
		}
	}
}

// TestRequestCanBeRecreatedAfterTerminalState is the other half of the
// deduplication rule: a request that failed must not block a new one forever,
// or a film deleted from Plex could never be requested again.
func TestRequestCanBeRecreatedAfterTerminalState(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")

	for _, state := range []string{"failed", "canceled", "completed", "skipped"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO requests (media_id, workflow_id, state, search_deadline)
			 VALUES ($1, 'movie-603', $2, now() + interval '2 years')`, mediaID, state); err != nil {
			t.Fatalf("insert terminal request in state %q: %v", state, err)
		}
	}

	// And now an active one is allowed again.
	if _, err := pool.Exec(ctx,
		`INSERT INTO requests (media_id, workflow_id, state, search_deadline)
		 VALUES ($1, 'movie-603', 'pending', now() + interval '2 years')`, mediaID); err != nil {
		t.Fatalf("active request after terminal ones: %v", err)
	}
}

func TestCascadeDeletesRequestChildren(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")
	userID := insertUser(t, pool, "alice")

	var requestID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO requests (media_id, workflow_id, state, search_deadline)
		 VALUES ($1, 'movie-603', 'pending', now() + interval '2 years')
		 RETURNING id`, mediaID).Scan(&requestID); err != nil {
		t.Fatalf("insert request: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO request_watchers (request_id, user_id) VALUES ($1, $2)`,
		requestID, userID); err != nil {
		t.Fatalf("insert watcher: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO request_events (request_id, event, to_state, actor)
		 VALUES ($1, 'created', 'pending', 'api')`, requestID); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM requests WHERE id = $1`, requestID); err != nil {
		t.Fatalf("delete request: %v", err)
	}

	for _, table := range []string{"request_watchers", "request_events"} {
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE request_id = $1`, requestID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s still has %d rows after the request was deleted", table, count)
		}
	}
}

func TestRequestViewJoinsMedia(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	mediaID := insertMedia(t, pool, "tv", 1399, ptr(int16(2)), "Game of Thrones")

	if _, err := pool.Exec(ctx,
		`INSERT INTO requests (media_id, workflow_id, state, search_deadline)
		 VALUES ($1, 'tv-1399-s2', 'pending', now() + interval '2 years')`, mediaID); err != nil {
		t.Fatalf("insert request: %v", err)
	}

	var (
		title     string
		season    sql.NullInt16
		mediaType string
		state     string
	)
	err := pool.QueryRow(ctx,
		`SELECT title, season, media_type, state FROM request_view`).
		Scan(&title, &season, &mediaType, &state)
	if err != nil {
		t.Fatalf("query view: %v", err)
	}

	if title != "Game of Thrones" || mediaType != "tv" || state != "pending" {
		t.Errorf("view returned %q/%q/%q", title, mediaType, state)
	}
	if !season.Valid || season.Int16 != 2 {
		t.Errorf("season = %v, want 2", season)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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

func insertUser(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()

	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (username) VALUES ($1) RETURNING id`, name).Scan(&id)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func ptr[T any](v T) *T { return &v }
