package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/alex/mediad/internal/domain"
)

// StoreModule wires the repositories into the fx graph.
var StoreModule = fx.Module("store", fx.Provide(NewStore))

// ErrNotFound is returned when a row the caller named does not exist.
//
// It is a sentinel rather than a nil result because most lookups here are for
// something that must exist — a request an activity was handed the id of — and
// a silent zero value would turn a missing row into a confusing downstream
// failure.
var ErrNotFound = errors.New("not found")

// Store is the repository layer over the application database.
//
// It is a plain struct of methods rather than an interface per entity: there
// is one database, the queries are all specific to this schema, and an
// interface would only be useful for mocking in tests that are better served
// by a real database anyway.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewStore builds the repository layer.
func NewStore(pool *pgxpool.Pool, log *slog.Logger) *Store {
	return &Store{pool: pool, log: log.With("component", "store")}
}

// Pool exposes the connection pool for callers that need a transaction of
// their own.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// inTx runs fn in a transaction, rolling back on error.
//
// Every write that touches more than one table goes through here. The
// projection is the reason: a state change and the event that records it have
// to land together, or the audit trail stops matching the state.
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		// Rollback after a successful commit is a no-op, so this is safe as an
		// unconditional defer.
		_ = tx.Rollback(ctx)
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Media
// ---------------------------------------------------------------------------

// UpsertMedia records a title and returns its row id.
//
// The insert conflicts on the identity index, so requesting the same film
// twice reuses one row. The metadata columns are refreshed on conflict: a
// title's Russian name or release date can be filled in on TMDB after the
// first request, and refusing to update would leave the project working from a
// stale copy for the two years a request can live.
//
// The Plex columns are deliberately not touched here: they belong to the
// existence check, which is the only thing that knows when it last asked.
func (s *Store) UpsertMedia(ctx context.Context, spec domain.MediaSpec) (int64, error) {
	const q = `
		INSERT INTO media (
			media_type, tmdb_id, season, title, title_ru, original_title,
			year, poster_path, overview, runtime_min, released_at, imdb_id, tvdb_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (media_type, tmdb_id, COALESCE(season, -1)) DO UPDATE SET
			title          = EXCLUDED.title,
			title_ru       = COALESCE(EXCLUDED.title_ru, media.title_ru),
			original_title = COALESCE(EXCLUDED.original_title, media.original_title),
			year           = COALESCE(EXCLUDED.year, media.year),
			poster_path    = COALESCE(EXCLUDED.poster_path, media.poster_path),
			overview       = COALESCE(EXCLUDED.overview, media.overview),
			runtime_min    = COALESCE(EXCLUDED.runtime_min, media.runtime_min),
			released_at    = COALESCE(EXCLUDED.released_at, media.released_at),
			imdb_id        = COALESCE(EXCLUDED.imdb_id, media.imdb_id),
			tvdb_id        = COALESCE(EXCLUDED.tvdb_id, media.tvdb_id),
			updated_at     = now()
		RETURNING id`

	var id int64
	err := s.pool.QueryRow(ctx, q,
		spec.Ref.Type, spec.Ref.TMDBID, spec.Ref.Season,
		spec.Title, nullString(spec.TitleRU), nullString(spec.OriginalTitle),
		nullInt(spec.Year), nullString(spec.PosterPath), nullString(spec.Overview),
		nullInt(spec.RuntimeMin), spec.ReleasedAt,
		nullString(spec.IMDBID), nullInt64(spec.TVDBID),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert media %s: %w", spec.Ref, err)
	}
	return id, nil
}

// Media is a row of the media table with its Plex status.
type Media struct {
	ID         int64
	Spec       domain.MediaSpec
	PosterPath string
	Overview   string

	InPlex        bool
	PlexRatingKey string
	PlexSection   string
	PlexTitle     string
	PlexYear      int
	// PlexCheckedAt is nil when the library has never been asked about this
	// title, which the search loop treats differently from "asked and absent".
	PlexCheckedAt *time.Time
}

// GetMedia loads a media row by id.
func (s *Store) GetMedia(ctx context.Context, id int64) (*Media, error) {
	const q = `
		SELECT id, media_type, tmdb_id, season, title, COALESCE(title_ru, ''),
		       COALESCE(original_title, ''), COALESCE(year, 0),
		       COALESCE(poster_path, ''), COALESCE(overview, ''),
		       COALESCE(runtime_min, 0), COALESCE(imdb_id, ''), COALESCE(tvdb_id, 0),
		       released_at,
		       in_plex, COALESCE(plex_rating_key, ''), COALESCE(plex_section_key, ''),
		       COALESCE(plex_title, ''), COALESCE(plex_year, 0),
		       plex_checked_at
		  FROM media WHERE id = $1`

	var (
		m          Media
		season     *int
		releasedAt *time.Time
	)
	err := s.pool.QueryRow(ctx, q, id).Scan(
		&m.ID, &m.Spec.Ref.Type, &m.Spec.Ref.TMDBID, &season,
		&m.Spec.Title, &m.Spec.TitleRU, &m.Spec.OriginalTitle, &m.Spec.Year,
		&m.Spec.PosterPath, &m.Spec.Overview,
		&m.Spec.RuntimeMin, &m.Spec.IMDBID, &m.Spec.TVDBID,
		&releasedAt,
		&m.InPlex, &m.PlexRatingKey, &m.PlexSection,
		&m.PlexTitle, &m.PlexYear, &m.PlexCheckedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("media %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get media %d: %w", id, err)
	}

	m.Spec.Ref.Season = season
	m.Spec.ReleasedAt = releasedAt

	// Kept as separate fields too, for the callers that want the poster of a
	// film without reaching through the spec.
	m.PosterPath = m.Spec.PosterPath
	m.Overview = m.Spec.Overview

	return &m, nil
}

// PlexStatus records the outcome of an existence check.
//
// A negative result is recorded too, and that matters: "we asked and it was not
// there" is different from "we have never asked", and the search loop re-checks
// on a TTL rather than on every run precisely because that distinction is
// stored.
func (s *Store) PlexStatus(ctx context.Context, mediaID int64, match *PlexMatch) error {
	const q = `
		UPDATE media SET
			in_plex          = $2,
			plex_rating_key  = $3,
			plex_section_key = $4,
			plex_title       = $5,
			plex_year        = $6,
			plex_added_at    = $7,
			plex_checked_at  = now(),
			updated_at       = now()
		WHERE id = $1`

	var (
		ratingKey, section, title *string
		year                      *int
		addedAt                   *time.Time
	)
	if match != nil {
		ratingKey = nullString(match.RatingKey)
		section = nullString(match.SectionKey)
		title = nullString(match.Title)
		year = nullInt(match.Year)
		if !match.AddedAt.IsZero() {
			addedAt = &match.AddedAt
		}
	}

	_, err := s.pool.Exec(ctx, q, mediaID, match != nil, ratingKey, section, title, year, addedAt)
	if err != nil {
		return fmt.Errorf("record plex status for media %d: %w", mediaID, err)
	}
	return nil
}

// PlexMatch is what the existence check found, as stored.
type PlexMatch struct {
	RatingKey  string
	SectionKey string
	Title      string
	Year       int
	AddedAt    time.Time
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// nullString turns an empty string into NULL.
//
// Worth doing rather than storing "": the schema uses NULL to mean "unknown",
// and COALESCE-based upserts above rely on that distinction to decide whether
// an incoming value should overwrite the stored one.
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

func nullInt64(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}
