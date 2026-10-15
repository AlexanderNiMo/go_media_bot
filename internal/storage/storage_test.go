package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alex/mediad/internal/domain"
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

// ---------------------------------------------------------------------------
// Restarting a request
// ---------------------------------------------------------------------------

// newStoreFor builds a Store over a throwaway database.
func newStoreFor(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()

	pool := newTestDB(t)
	return NewStore(pool, slog.New(slog.DiscardHandler)), pool
}

// failedRequest returns a request left in the state a retry has to clean up.
func failedRequest(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")

	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO requests (
			media_id, workflow_id, state, attempt_count, error_streak, stall_count,
			last_error, next_run_at, search_deadline, download_deadline,
			selected_title, selected_indexer, selected_guid, selected_infohash,
			selected_size, selected_seeders, selected_score, selected_at,
			qbit_hash, content_path, progress, dl_speed, eta_seconds, zero_since,
			missing_since, qbit_state, completed_at
		) VALUES (
			$1, 'movie-603', 'failed', 47, 3, 3,
			'three releases stalled', now() - interval '1 hour', now() + interval '1 day',
			now() - interval '2 days',
			'Матрица (1999) 1080p BDRemux', 'RuTracker', 'guid-1', 'abc123',
			20000000000, 51, 0.87, now() - interval '3 days',
			'abc123', '/downloads/Matrix', 0.43, 1200000, 4800, now() - interval '3 days',
			now() - interval '3 days', 'stalledDL', now() - interval '1 day'
		) RETURNING id`, mediaID).Scan(&id)
	if err != nil {
		t.Fatalf("insert failed request: %v", err)
	}
	return id
}

// TestRestartRequestClearsTheProcess is about what the row means afterwards.
//
// A retry is not a resumed download: the torrent is gone, the release it named
// is not being fetched, and the counters describe a search that has ended. A
// row that kept any of it would have `status` reporting a download that is not
// happening.
func TestRestartRequestClearsTheProcess(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	id := failedRequest(t, ctx, pool)

	nextRunAt := time.Now().Truncate(time.Millisecond)
	deadline := nextRunAt.Add(2 * 365 * 24 * time.Hour)

	got, err := store.RestartRequest(ctx, RestartInput{
		ID:               id,
		FromState:        StateFailed,
		PreviousAttempts: 47,
		PreviousError:    "three releases stalled",
		NextRunAt:        nextRunAt,
		SearchDeadline:   deadline,
		Actor:            "cli",
		RequestedBy:      "alex",
	})
	if err != nil {
		t.Fatalf("RestartRequest: %v", err)
	}

	if got.State != StatePending {
		t.Errorf("state = %q, want pending", got.State)
	}
	if got.AttemptCount != 0 || got.StallCount != 0 {
		t.Errorf("counters survived: attempts=%d stalls=%d", got.AttemptCount, got.StallCount)
	}
	if got.LastError != "" {
		t.Errorf("last error survived: %q", got.LastError)
	}
	if got.QbitHash != "" || got.ContentPath != "" {
		t.Errorf("the torrent address survived: hash=%q path=%q", got.QbitHash, got.ContentPath)
	}
	if got.SelectedTitle != "" {
		t.Errorf("the selected release survived: %q", got.SelectedTitle)
	}
	if got.Progress != 0 {
		t.Errorf("progress survived: %v", got.Progress)
	}
	if got.DownloadDeadline != nil {
		t.Errorf("the download deadline survived: %v", got.DownloadDeadline)
	}
	if got.CompletedAt != nil {
		t.Errorf("the completion time survived: %v", got.CompletedAt)
	}

	// The window is replaced rather than resumed, which is the point: a retry
	// that inherited a deadline in the past would fail on its first pass.
	if !got.SearchDeadline.After(time.Now().Add(300 * 24 * time.Hour)) {
		t.Errorf("search deadline = %v, want a fresh window", got.SearchDeadline)
	}
	if got.NextRunAt == nil || got.NextRunAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("next run = %v, want it due now", got.NextRunAt)
	}
}

// TestRestartRequestRecordsWhatItReplaced — the reset clears the columns, so
// the event is the only place the previous outcome survives.
func TestRestartRequestRecordsWhatItReplaced(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	id := failedRequest(t, ctx, pool)

	if _, err := store.RestartRequest(ctx, RestartInput{
		ID:               id,
		FromState:        StateFailed,
		PreviousAttempts: 47,
		PreviousError:    "three releases stalled",
		NextRunAt:        time.Now(),
		SearchDeadline:   time.Now().Add(time.Hour),
		Actor:            "cli",
		RequestedBy:      "alex",
	}); err != nil {
		t.Fatalf("RestartRequest: %v", err)
	}

	events, err := store.ListEvents(ctx, id, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	e := events[0]
	if e.Event != "request.retried" {
		t.Errorf("event = %q", e.Event)
	}
	if e.FromState != StateFailed || e.ToState != StatePending {
		t.Errorf("transition recorded as %q -> %q", e.FromState, e.ToState)
	}
	if e.Actor != "cli" {
		t.Errorf("actor = %q, want the interface that asked", e.Actor)
	}
	if !strings.Contains(string(e.Detail), "alex") {
		t.Errorf("the person is not on the detail: %s", e.Detail)
	}

	detail := string(e.Detail)
	for _, want := range []string{"three releases stalled", "47"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the previous outcome is not in the detail: %s", detail)
		}
	}
}

func TestRestartRequestOnAMissingRequest(t *testing.T) {
	store, _ := newStoreFor(t)

	_, err := store.RestartRequest(context.Background(), RestartInput{
		ID:             999,
		NextRunAt:      time.Now(),
		SearchDeadline: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestRestartRequestAfterACompletedDownload — the other reason to retry is
// that the file was deleted from Plex, and the row has to be usable again.
func TestRestartRequestAfterACompletedDownload(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	id := failedRequest(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE requests SET state = 'completed' WHERE id = $1`, id); err != nil {
		t.Fatalf("mark completed: %v", err)
	}

	got, err := store.RestartRequest(ctx, RestartInput{
		ID:             id,
		FromState:      StateCompleted,
		NextRunAt:      time.Now(),
		SearchDeadline: time.Now().Add(time.Hour),
		Actor:          "cli",
	})
	if err != nil {
		t.Fatalf("RestartRequest: %v", err)
	}
	if got.State != StatePending {
		t.Errorf("state = %q, want pending", got.State)
	}
}

// ---------------------------------------------------------------------------
// Finding a request
// ---------------------------------------------------------------------------

func TestFindRequestByWorkflowIDPrefersTheActiveOne(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")

	// A failed request and then a fresh one under the same workflow id, which
	// is what --force leaves behind: several rows, one id.
	if _, err := pool.Exec(ctx,
		`INSERT INTO requests (media_id, workflow_id, state, search_deadline)
		 VALUES ($1, 'movie-603', 'failed', now() + interval '2 years')`, mediaID); err != nil {
		t.Fatalf("insert failed request: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO requests (media_id, workflow_id, state, search_deadline)
		 VALUES ($1, 'movie-603', 'downloading', now() + interval '2 years')`, mediaID); err != nil {
		t.Fatalf("insert active request: %v", err)
	}

	got, err := store.FindRequestByWorkflowID(ctx, "movie-603")
	if err != nil {
		t.Fatalf("FindRequestByWorkflowID: %v", err)
	}
	if got.State != StateDownloading {
		t.Errorf("resolved to the %q row; a person asking about movie-603 means the live one", got.State)
	}
}

func TestFindRequestByWorkflowIDWhenThereIsNone(t *testing.T) {
	store, _ := newStoreFor(t)

	if _, err := store.FindRequestByWorkflowID(context.Background(), "movie-999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

func TestListRequestsFilters(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	// One of each state, on distinct media so the active-uniqueness index is
	// not in the way.
	states := []State{
		StatePending, StateSearching, StateAdding, StateDownloading,
		StateCompleted, StateFailed, StateCanceled, StateSkipped,
	}
	for i, state := range states {
		mediaID := insertMedia(t, pool, "movie", int64(1000+i), nil, "Title")
		// Stamps a poster on every media row so the request_view reads it
		// back into the projection's PosterPath. The fixture value is a
		// path-shaped string; nothing here renders it.
		if _, err := pool.Exec(ctx,
			`UPDATE media SET poster_path = $1 WHERE id = $2`,
			"/abc"+strconv.Itoa(1000+i)+".jpg", mediaID); err != nil {
			t.Fatalf("poster: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO requests (media_id, workflow_id, state, search_deadline)
			 VALUES ($1, $2, $3, now() + interval '2 years')`,
			mediaID, "movie-"+strconv.Itoa(1000+i), state); err != nil {
			t.Fatalf("insert %s: %v", state, err)
		}
	}

	t.Run("active only", func(t *testing.T) {
		got, err := store.ListRequests(ctx, RequestFilter{States: ActiveStates()})
		if err != nil {
			t.Fatalf("ListRequests: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("got %d requests, want the 4 active ones", len(got))
		}
		for _, r := range got {
			if !r.State.Active() {
				t.Errorf("a %q request came back from the active filter", r.State)
			}
		}
	})

	t.Run("poster on every request", func(t *testing.T) {
		got, err := store.ListRequests(ctx, RequestFilter{})
		if err != nil {
			t.Fatalf("ListRequests: %v", err)
		}
		if len(got) != len(states) {
			t.Fatalf("got %d requests, want %d", len(got), len(states))
		}
		// Every request came back with the poster its media was stamped
		// with. The exact format is "/abc<TMDBID>.jpg", so the test is
		// independent of how the projection orders its joins.
		for _, r := range got {
			if !strings.HasPrefix(r.PosterPath, "/abc") || !strings.HasSuffix(r.PosterPath, ".jpg") {
				t.Errorf("request %d (media_id=%d, tmdb_id=%d) poster = %q, want /abc<TMDBID>.jpg",
					r.ID, r.MediaID, r.Media.Ref.TMDBID, r.PosterPath)
			}
		}
	})

	t.Run("everything", func(t *testing.T) {
		got, err := store.ListRequests(ctx, RequestFilter{})
		if err != nil {
			t.Fatalf("ListRequests: %v", err)
		}
		if len(got) != len(states) {
			t.Errorf("got %d requests, want %d", len(got), len(states))
		}
	})

	t.Run("one state", func(t *testing.T) {
		got, err := store.ListRequests(ctx, RequestFilter{States: []State{StateFailed}})
		if err != nil {
			t.Fatalf("ListRequests: %v", err)
		}
		if len(got) != 1 || got[0].State != StateFailed {
			t.Errorf("got %d requests, want just the failed one", len(got))
		}
	})

	t.Run("limit", func(t *testing.T) {
		got, err := store.ListRequests(ctx, RequestFilter{Limit: 2})
		if err != nil {
			t.Fatalf("ListRequests: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d requests, want 2", len(got))
		}
	})

	t.Run("by media type", func(t *testing.T) {
		got, err := store.ListRequests(ctx, RequestFilter{MediaType: "tv"})
		if err != nil {
			t.Fatalf("ListRequests: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d series, want none", len(got))
		}
	})

	t.Run("by watcher", func(t *testing.T) {
		userID := insertUser(t, pool, "alex")

		watched, err := store.ListRequests(ctx, RequestFilter{})
		if err != nil {
			t.Fatalf("ListRequests: %v", err)
		}
		if err := store.AddWatcher(ctx, watched[0].ID, userID); err != nil {
			t.Fatalf("AddWatcher: %v", err)
		}

		got, err := store.ListRequests(ctx, RequestFilter{WatcherUserID: userID})
		if err != nil {
			t.Fatalf("ListRequests: %v", err)
		}
		if len(got) != 1 || got[0].ID != watched[0].ID {
			t.Errorf("got %d requests, want only the watched one", len(got))
		}

		// Somebody else watches nothing.
		otherID := insertUser(t, pool, "someone-else")
		got, err = store.ListRequests(ctx, RequestFilter{WatcherUserID: otherID})
		if err != nil {
			t.Fatalf("ListRequests: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d requests for a user watching nothing", len(got))
		}
	})

	t.Run("newest first", func(t *testing.T) {
		got, err := store.ListRequests(ctx, RequestFilter{})
		if err != nil {
			t.Fatalf("ListRequests: %v", err)
		}
		for i := 1; i < len(got); i++ {
			if got[i].CreatedAt.After(got[i-1].CreatedAt) {
				t.Fatalf("row %d is newer than the one before it", i)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

func TestEnsureUserByCLI(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	first, err := store.EnsureUserByCLI(ctx, "alex")
	if err != nil {
		t.Fatalf("EnsureUserByCLI: %v", err)
	}
	if first.Username != "alex" {
		t.Errorf("username = %q", first.Username)
	}
	if first.IsAdmin {
		t.Error("a CLI user was created as an administrator")
	}

	// Asking twice is what happens on every command, so it must not create a
	// second account.
	again, err := store.EnsureUserByCLI(ctx, "alex")
	if err != nil {
		t.Fatalf("EnsureUserByCLI again: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("got a second account: %d then %d", first.ID, again.ID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Errorf("%d user rows, want 1", count)
	}
}

// TestEnsureUserByCLIWhenTheNameIsTaken — the same person may already exist
// from Telegram or the web under the same name, and the account is created
// without one rather than failing or, worse, adopting the existing account:
// linking two identities is something a person does with a code, because
// otherwise anyone who can run the CLI as a given user takes over that
// account.
func TestEnsureUserByCLIWhenTheNameIsTaken(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	taken := insertUser(t, pool, "alex")

	got, err := store.EnsureUserByCLI(ctx, "alex")
	if err != nil {
		t.Fatalf("EnsureUserByCLI: %v", err)
	}
	if got.ID == taken {
		t.Fatal("the CLI identity adopted the existing account")
	}
	if got.Username != "" {
		t.Errorf("username = %q, want it left unset", got.Username)
	}

	// And it is stable: the next command finds the same nameless account
	// rather than creating another.
	again, err := store.EnsureUserByCLI(ctx, "alex")
	if err != nil {
		t.Fatalf("EnsureUserByCLI again: %v", err)
	}
	if again.ID != got.ID {
		t.Errorf("got %d then %d", got.ID, again.ID)
	}
}

func TestFindUserByCLIDoesNotCreate(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	if _, err := store.FindUserByCLI(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Errorf("a read created %d users", count)
	}
}

// TestNotificationsCarryNoPayload is a regression test for a bug that would
// have wedged every request that ever finished.
//
// The payload column is NOT NULL DEFAULT '{}', and a DEFAULT only applies when
// the column is omitted — passing an explicit NULL overrides it and violates
// the constraint. Every caller with nothing to attach passes nil, including
// the workflow, which never sets one.
//
// The reason it was severe rather than cosmetic is the retry policy on the
// notification activity: it retries forever by design, because being told the
// thing you asked for is ready is the entire point of the project. So a
// completed download would retry the same doomed insert every five minutes for
// the rest of its two-year lifetime, and the request would never complete.
func TestNotificationsCarryNoPayload(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")
	userID := insertUser(t, pool, "alex")

	var requestID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO requests (media_id, workflow_id, state, search_deadline)
		VALUES ($1, 'movie-603', 'downloading', now() + interval '2 years')
		RETURNING id`, mediaID).Scan(&requestID); err != nil {
		t.Fatalf("insert request: %v", err)
	}
	if err := store.AddWatcher(ctx, requestID, userID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}

	// A nil payload, which is what the workflow passes.
	created, err := store.CreateNotifications(ctx, requestID,
		NotificationCompleted, "Матрица готова", "", nil)
	if err != nil {
		t.Fatalf("CreateNotifications with no payload: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created %d notifications, want 1", len(created))
	}

	// And the payload is stored as an object rather than as SQL NULL.
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM notifications WHERE id = $1`, created[0].ID).Scan(&payload); err != nil {
		t.Fatalf("read the payload: %v", err)
	}
	if string(payload) != "{}" {
		t.Errorf("payload = %q, want {}", payload)
	}
}

// TestNotificationsSkipWatchersWhoOptedOut — the mask is what stops a
// notification kind from being delivered to somebody who does not want it,
// while the kinds that always go out still do.
func TestNotificationsSkipWatchersWhoOptedOut(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")
	quiet := insertUser(t, pool, "quiet")
	normal := insertUser(t, pool, "normal")

	var requestID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO requests (media_id, workflow_id, state, search_deadline)
		VALUES ($1, 'movie-603', 'downloading', now() + interval '2 years')
		RETURNING id`, mediaID).Scan(&requestID); err != nil {
		t.Fatalf("insert request: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO request_watchers (request_id, user_id, notify_mask)
		VALUES ($1, $2, 0), ($1, $3, 7)`, requestID, quiet, normal); err != nil {
		t.Fatalf("insert watchers: %v", err)
	}

	created, err := store.CreateNotifications(ctx, requestID,
		NotificationCompleted, "Готово", "", nil)
	if err != nil {
		t.Fatalf("CreateNotifications: %v", err)
	}

	// Only the watcher who wants this kind.
	if len(created) != 1 {
		t.Fatalf("created %d notifications, want 1", len(created))
	}
	if created[0].UserID != normal {
		t.Errorf("the notification went to user %d, want %d", created[0].UserID, normal)
	}
}

// TestListTelegramIdentitiesReturnsTheUsersChats is the round-trip the
// settings page reads to render the linked list. A user with no chat returns
// nil; a user with one returns it; the chat belongs to the user it claims
// to.
func TestListTelegramIdentitiesReturnsTheUsersChats(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	uid := insertUser(t, pool, "alex")
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_identities (user_id, provider, external_id)
		VALUES ($1, 'telegram', '111'), ($1, 'telegram', '222')`, uid); err != nil {
		t.Fatalf("link: %v", err)
	}

	ids, err := store.ListTelegramIdentities(ctx, uid)
	if err != nil {
		t.Fatalf("ListTelegramIdentities: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d identities, want 2", len(ids))
	}
	if ids[0].ExternalID != "111" || ids[1].ExternalID != "222" {
		t.Errorf("ids = [%s %s], want [111 222]", ids[0].ExternalID, ids[1].ExternalID)
	}
}

// TestUnlinkTelegramRemovesOnlyTheMatchingChat is the safety net: a
// cross-account delete would be a serious bug, so the WHERE clause is
// user_id + external_id, and a request for somebody else's chat returns
// "not found" without changing anything.
func TestUnlinkTelegramRemovesOnlyTheMatchingChat(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()

	alex := insertUser(t, pool, "alex")
	bob := insertUser(t, pool, "bob")
	for _, u := range []struct {
		uid  int64
		chat string
	}{{alex, "alex-1"}, {bob, "bob-1"}} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO user_identities (user_id, provider, external_id)
			VALUES ($1, 'telegram', $2)`, u.uid, u.chat); err != nil {
			t.Fatalf("link: %v", err)
		}
	}

	removed, err := store.UnlinkTelegram(ctx, alex, "bob-1")
	if err != nil {
		t.Fatalf("UnlinkTelegram: %v", err)
	}
	if removed {
		t.Error("alex was allowed to remove bob's chat")
	}

	// The reverse: alex unlinking his own chat works, bob's stays.
	removed, err = store.UnlinkTelegram(ctx, alex, "alex-1")
	if err != nil || !removed {
		t.Fatalf("UnlinkTelegram own chat: removed=%v err=%v", removed, err)
	}

	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM user_identities WHERE user_id = $1`, bob).
		Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Errorf("bob's chats remaining = %d, want 1", remaining)
	}
}

// TestTelegramNotifyMaskRoundTrip — the default is 15 (all bits), the
// setter clamps out-of-range, and a freshly read row returns what was
// written.
func TestTelegramNotifyMaskRoundTrip(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()
	uid := insertUser(t, pool, "alex")

	got, err := store.GetTelegramNotifyMask(ctx, uid)
	if err != nil {
		t.Fatalf("GetTelegramNotifyMask: %v", err)
	}
	if got != 15 {
		t.Errorf("default mask = %d, want 15", got)
	}

	if err := store.SetTelegramNotifyMask(ctx, uid, 5); err != nil {
		t.Fatalf("SetTelegramNotifyMask: %v", err)
	}
	got, err = store.GetTelegramNotifyMask(ctx, uid)
	if err != nil {
		t.Fatalf("GetTelegramNotifyMask: %v", err)
	}
	if got != 5 {
		t.Errorf("after write, mask = %d, want 5", got)
	}

	if err := store.SetTelegramNotifyMask(ctx, uid, 16); err == nil {
		t.Error("SetTelegramNotifyMask accepted 16, want an out-of-range error")
	}
	if err := store.SetTelegramNotifyMask(ctx, uid, -1); err == nil {
		t.Error("SetTelegramNotifyMask accepted -1, want an out-of-range error")
	}
}

// TestCandidatesRoundTrip covers the four-step dance the manual-pick
// flow does: store a list, list it back, mark one as picked, clear the
// rest. The order matters: StoreCandidates is the workflow's
// "I have a list" write, ListCandidates is the web page read,
// GetPickedCandidate is the workflow's poll for a pick, and
// ClearCandidates is the cleanup before downloadCycle takes over.
func TestCandidatesRoundTrip(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()
	uid := insertUser(t, pool, "alex")
	rid := insertTestRequest(t, pool, ctx, store, uid, "tv-1-s1", 9999)

	// Empty list: ListCandidates returns nil, not an error. A workflow
	// that runs before the first manual-mode entry needs to be able
	// to ask "any candidates yet?" without having to special-case
	// the not-found path.
	got, err := store.ListCandidates(ctx, rid)
	if err != nil {
		t.Fatalf("ListCandidates on empty: %v", err)
	}
	if got != nil {
		t.Errorf("ListCandidates on empty = %v, want nil", got)
	}

	// Store a list of three.
	original := []Candidate{
		{Rank: 1, Title: "Release A", Indexer: "rutor", InfoHash: "h1", MagnetLink: "magnet:?xt=1", SizeBytes: 1024, Seeders: 5, RejectionReason: "russian_audio"},
		{Rank: 2, Title: "Release B", Indexer: "rutor", InfoHash: "h2", MagnetLink: "magnet:?xt=2", SizeBytes: 2048, Seeders: 3, RejectionReason: "size_under"},
		{Rank: 3, Title: "Release C", Indexer: "rutor", InfoHash: "h3", MagnetLink: "magnet:?xt=3", SizeBytes: 4096, Seeders: 8, RejectionReason: "quality"},
	}
	if err := store.StoreCandidates(ctx, rid, original); err != nil {
		t.Fatalf("StoreCandidates: %v", err)
	}

	// List comes back in rank order with the same fields. The
	// round-trip uses the same struct, so a field that is dropped
	// or reordered surfaces here.
	got, err = store.ListCandidates(ctx, rid)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListCandidates = %d, want 3", len(got))
	}
	for i, c := range got {
		if c.Rank != i+1 {
			t.Errorf("got[%d].Rank = %d, want %d", i, c.Rank, i+1)
		}
		if c.Title != original[i].Title {
			t.Errorf("got[%d].Title = %q, want %q", i, c.Title, original[i].Title)
		}
		if c.MagnetLink != original[i].MagnetLink {
			t.Errorf("got[%d].MagnetLink = %q, want %q", i, c.MagnetLink, original[i].MagnetLink)
		}
		if c.PickedAt != nil {
			t.Errorf("got[%d].PickedAt = %v, want nil before pick", i, c.PickedAt)
		}
	}

	// Storing a new list replaces the old one entirely. The manual-mode
	// path does this on every search tick that finds a fresh batch.
	if err := store.StoreCandidates(ctx, rid, []Candidate{
		{Rank: 1, Title: "Release X", Indexer: "rutor", InfoHash: "hX", MagnetLink: "magnet:?xt=X", SizeBytes: 100, Seeders: 1, RejectionReason: "ok"},
	}); err != nil {
		t.Fatalf("StoreCandidates (replace): %v", err)
	}
	got, _ = store.ListCandidates(ctx, rid)
	if len(got) != 1 || got[0].Title != "Release X" {
		t.Errorf("after replace: %d items, %v", len(got), got)
	}

	// Mark a candidate picked and re-store the original three.
	if err := store.StoreCandidates(ctx, rid, original); err != nil {
		t.Fatalf("StoreCandidates (restore): %v", err)
	}
	picked, err := store.MarkPicked(ctx, rid, 2)
	if err != nil {
		t.Fatalf("MarkPicked: %v", err)
	}
	if picked.Rank != 2 || picked.Title != "Release B" {
		t.Errorf("MarkPicked returned rank %d title %q, want 2/\"Release B\"", picked.Rank, picked.Title)
	}

	// The poll for the picked row should see the same record, and
	// only the picked row.
	var pickedAfterPoll Candidate
	pickedAfterPoll, err = store.GetPickedCandidate(ctx, rid)
	if err != nil {
		t.Fatalf("GetPickedCandidate: %v", err)
	}
	if pickedAfterPoll.Rank != 2 || pickedAfterPoll.Title != "Release B" {
		t.Errorf("GetPickedCandidate = rank %d title %q, want 2/\"Release B\"", pickedAfterPoll.Rank, pickedAfterPoll.Title)
	}
	if pickedAfterPoll.PickedAt == nil {
		t.Error("GetPickedCandidate.PickedAt is nil, want a timestamp")
	}

	// The list still has all three rows, but the picked one has a
	// non-nil PickedAt. The web UI uses that to render a "you picked
	// this" badge; the workflow uses the picked row to build the
	// download.
	got, _ = store.ListCandidates(ctx, rid)
	if len(got) != 3 {
		t.Fatalf("after pick, len = %d, want 3", len(got))
	}
	var pickedCount int
	for _, c := range got {
		if c.PickedAt != nil {
			pickedCount++
		}
	}
	if pickedCount != 1 {
		t.Errorf("picked rows = %d, want 1", pickedCount)
	}

	// ClearCandidates wipes the list. The workflow calls this on
	// the manual-mode timeout and on a successful pick. After
	// clear, the picked row is gone and the list is empty.
	if err := store.ClearCandidates(ctx, rid); err != nil {
		t.Fatalf("ClearCandidates: %v", err)
	}
	got, _ = store.ListCandidates(ctx, rid)
	if got != nil {
		t.Errorf("ListCandidates after clear = %v, want nil", got)
	}
	if _, err := store.GetPickedCandidate(ctx, rid); !errors.Is(err, ErrNoSuchCandidate) {
		t.Errorf("GetPickedCandidate after clear = %v, want ErrNoSuchCandidate", err)
	}
}

// TestMarkPickedOnEmptyList — a wrong rank returns ErrNoSuchCandidate.
// The web handler maps this to a 404, the bot maps it to a polite
// "no such candidate" message. Both paths depend on the typed error.
func TestMarkPickedOnEmptyList(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()
	uid := insertUser(t, pool, "alex")
	rid := insertTestRequest(t, pool, ctx, store, uid, "tv-2-s1", 9999)

	if _, err := store.MarkPicked(ctx, rid, 1); !errors.Is(err, ErrNoSuchCandidate) {
		t.Errorf("MarkPicked on empty list = %v, want ErrNoSuchCandidate", err)
	}
}

// insertTestRequest creates a request the candidate tests can attach
// candidates to. It exists in this file rather than the shared
// helpers because the other tests build their requests through
// CreateRequestInput and the wiring that goes with it; the candidate
// tests only need a row to hang candidates off.
func insertTestRequest(t *testing.T, pool *pgxpool.Pool, ctx context.Context, store *Store, uid int64, workflowID string, tmdbID int64) int64 {
	t.Helper()
	var season int
	req, err := store.CreateRequest(ctx, CreateRequestInput{
		WorkflowID:     workflowID,
		FirstRunID:     workflowID + "-run-1",
		SearchDeadline: time.Now().Add(48 * time.Hour),
		NextRunAt:      time.Now().Add(time.Minute),
		Spec: domain.MediaSpec{
			Ref:    domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: tmdbID, Season: &season},
			Title:  workflowID,
			Year:   1999,
		},
	})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if err := store.AddWatcher(ctx, req.ID, uid); err != nil {
		t.Fatalf("AddWatcher: %v", err)
	}
	return req.ID
}
