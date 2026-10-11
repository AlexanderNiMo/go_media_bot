package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/alex/mediad/internal/domain"
)

// State is the phase a request is in.
//
// It mirrors the CHECK constraint on requests.state, and the values are the
// same strings. The database is the enforcer: an unknown state is rejected at
// write time rather than discovered later by a UI that cannot render it.
type State string

const (
	StatePending     State = "pending"
	StateSearching   State = "searching"
	StateAdding      State = "adding"
	StateDownloading State = "downloading"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
	StateCanceled    State = "canceled"
	StateSkipped     State = "skipped"
)

// Active reports whether a state means the request is still being worked on.
func (s State) Active() bool {
	switch s {
	case StatePending, StateSearching, StateAdding, StateDownloading:
		return true
	default:
		return false
	}
}

// Request is the projection row the UI and the CLI read.
//
// It is a denormalised join of requests and media, which is the point: nothing
// on a render path may talk to Temporal, because SQL-backed visibility has no
// ORDER BY at all, is eventually consistent, and a query needs a live worker.
type Request struct {
	ID         int64
	MediaID    int64
	WorkflowID string
	FirstRunID string
	RunID      string

	State State

	AttemptCount int
	ErrorStreak  int
	StallCount   int
	LastError    string

	NextRunAt        *time.Time
	SearchDeadline   time.Time
	DownloadDeadline *time.Time

	SelectedTitle   string
	SelectedIndexer string
	SelectedSize    int64
	SelectedSeeders int
	SelectedScore   float64

	QbitHash    string
	ContentPath string

	Progress   float64
	DlSpeed    int64
	ETASeconds int64

	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time

	Media domain.MediaSpec

	PosterPath string
	Overview   string

	InPlex    bool
	PlexTitle string
	PlexYear  int
}

// CreateRequestInput is what the service layer supplies when a request starts.
type CreateRequestInput struct {
	Spec       domain.MediaSpec
	WorkflowID string
	FirstRunID string
	// SearchDeadline bounds how long the request keeps looking.
	SearchDeadline time.Time
	// NextRunAt is when the first search is due. It carries jitter, so that
	// requests created together do not align forever.
	NextRunAt time.Time
}

// CreateRequest creates the media row if needed and then the request.
//
// The two writes are one transaction, and the returning id is what the caller
// starts the workflow with. A caller that instead started the workflow first
// and wrote the projection second would, on a crash in between, leave a
// running workflow that nothing can find.
func (s *Store) CreateRequest(ctx context.Context, in CreateRequestInput) (*Request, error) {
	var out *Request

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		mediaID, err := upsertMediaTx(ctx, tx, in.Spec)
		if err != nil {
			return err
		}

		const q = `
			INSERT INTO requests (
				media_id, workflow_id, first_run_id, state,
				next_run_at, search_deadline
			) VALUES ($1, $2, $3, 'pending', $4, $5)
			RETURNING id, created_at, updated_at`

		var (
			id        int64
			createdAt time.Time
			updatedAt time.Time
		)
		err = tx.QueryRow(ctx, q,
			mediaID, in.WorkflowID, nullString(in.FirstRunID),
			in.NextRunAt, in.SearchDeadline,
		).Scan(&id, &createdAt, &updatedAt)
		if err != nil {
			// The partial unique index on active requests is the arbiter for
			// "this media is already being fetched". Surfacing it as a
			// distinct error lets the caller attach a watcher to the existing
			// request instead of failing.
			if isUniqueViolation(err) {
				return ErrActiveRequestExists
			}
			return fmt.Errorf("insert request: %w", err)
		}

		out = &Request{
			ID:             id,
			MediaID:        mediaID,
			WorkflowID:     in.WorkflowID,
			FirstRunID:     in.FirstRunID,
			State:          StatePending,
			NextRunAt:      &in.NextRunAt,
			SearchDeadline: in.SearchDeadline,
			CreatedAt:      createdAt,
			UpdatedAt:      updatedAt,
			Media:          in.Spec,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ErrActiveRequestExists is returned when a request for the same media is
// already being worked on.
var ErrActiveRequestExists = errors.New("an active request for this media already exists")

// FindActiveRequest returns the active request for a media reference.
//
// It returns ErrNotFound when there is none, which is the ordinary case.
func (s *Store) FindActiveRequest(ctx context.Context, ref domain.MediaRef) (*Request, error) {
	const q = `
		SELECT r.id, r.workflow_id
		  FROM requests r
		  JOIN media m ON m.id = r.media_id
		 WHERE m.media_type = $1
		   AND m.tmdb_id = $2
		   AND COALESCE(m.season, -1) = COALESCE($3, -1)
		   AND r.state IN ('pending','searching','adding','downloading')`

	var (
		id         int64
		workflowID string
	)
	err := s.pool.QueryRow(ctx, q, ref.Type, ref.TMDBID, ref.Season).Scan(&id, &workflowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find active request: %w", err)
	}

	return &Request{ID: id, WorkflowID: workflowID}, nil
}

// GetRequest loads the projection for one request.
func (s *Store) GetRequest(ctx context.Context, id int64) (*Request, error) {
	const q = `SELECT ` + requestViewColumns + ` FROM request_view WHERE id = $1`

	r, err := scanRequest(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("request %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get request %d: %w", id, err)
	}
	return r, nil
}

// ListRequests returns requests, newest first.
//
// state filters to one state when non-empty; limit is clamped to something
// sane so a caller cannot ask for the whole table by accident.
func (s *Store) ListRequests(ctx context.Context, state State, limit int) ([]*Request, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	q := `SELECT ` + requestViewColumns + ` FROM request_view`
	args := []any{}
	if state != "" {
		q += ` WHERE state = $1`
		args = append(args, state)
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	defer rows.Close()

	var out []*Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("scan request: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestState is the subset of the projection the workflow itself writes.
//
// It is deliberately narrow. The workflow owns the process — which phase a
// request is in, when the next attempt is due, which release was chosen — and
// nothing else about the row is its business. Progress and the rest of what
// the torrent client reports belong to the download watcher; see
// UpdateDownloadFacts.
type RequestState struct {
	ID          int64
	State       State
	Attempt     int
	ErrorStreak int
	StallCount  int
	LastError   string

	NextRunAt        *time.Time
	DownloadDeadline *time.Time

	SelectedTitle    string
	SelectedIndexer  string
	SelectedGUID     string
	SelectedInfoHash string
	SelectedSize     int64
	SelectedSeeders  int
	SelectedScore    float64

	QbitHash    string
	ContentPath string

	Progress   float64
	DlSpeed    int64
	ETASeconds int64
	ZeroSince  *time.Time
}

// UpdateState writes the process columns of a request.
//
// It is the single write path for everything the workflow decides, which is
// what keeps the projection explainable: if a column here is wrong, the
// workflow put it wrong, and there is exactly one place to look.
func (s *Store) UpdateState(ctx context.Context, st RequestState) error {
	return updateStateTx(ctx, s.pool, st)
}

func updateStateTx(ctx context.Context, q querier, st RequestState) error {
	const sql = `
		UPDATE requests SET
			state             = $2,
			attempt_count     = $3,
			error_streak      = $4,
			stall_count       = $5,
			last_error        = $6,
			next_run_at       = $7,
			download_deadline = $8,
			selected_title    = $9,
			selected_indexer  = $10,
			selected_guid     = $11,
			selected_infohash = $12,
			selected_size     = $13,
			selected_seeders  = $14,
			selected_score    = $15,
			selected_at       = CASE WHEN $11 <> '' THEN now() ELSE selected_at END,
			qbit_hash         = $16,
			-- progress, dl_speed, eta_seconds, zero_since, missing_since,
			-- qbit_state and content_path are deliberately absent: the download
			-- watcher owns them, and this statement runs on every state change
			-- with a struct that never sets them. Writing them here would reset
			-- the progress of an in-flight download to zero every time the
			-- workflow recorded anything.
			completed_at      = CASE WHEN $2 IN ('completed','failed','canceled','skipped')
			                         THEN COALESCE(completed_at, now())
			                         ELSE completed_at END,
			updated_at        = now()
		WHERE id = $1`

	_, err := q.Exec(ctx, sql,
		st.ID, st.State, st.Attempt, st.ErrorStreak, st.StallCount,
		nullString(st.LastError), st.NextRunAt, st.DownloadDeadline,
		nullString(st.SelectedTitle), nullString(st.SelectedIndexer),
		nullString(st.SelectedGUID), nullString(st.SelectedInfoHash),
		nullInt64(st.SelectedSize), nullInt(st.SelectedSeeders), st.SelectedScore,
		nullString(st.QbitHash),
	)
	if err != nil {
		return fmt.Errorf("update request %d: %w", st.ID, err)
	}
	return nil
}

// SetRunID records the Temporal run id on a request.
//
// The run id identifies this particular execution, where the workflow id
// identifies the request across every execution it will ever have — including
// the ones a ContinueAsNew creates. The interface needs the run id to link to
// a specific run in the Temporal UI, which is where an operator goes to find
// out what a stuck request is doing.
func (s *Store) SetRunID(ctx context.Context, requestID int64, runID, workflowID string) error {
	const q = `
		UPDATE requests
		   SET run_id       = $2,
		       first_run_id = COALESCE(first_run_id, $2),
		       workflow_id  = $3,
		       updated_at   = now()
		 WHERE id = $1`

	if _, err := s.pool.Exec(ctx, q, requestID, nullString(runID), workflowID); err != nil {
		return fmt.Errorf("record run id for request %d: %w", requestID, err)
	}
	return nil
}

// MediaIDForRequest returns the media row a request points at.
func (s *Store) MediaIDForRequest(ctx context.Context, requestID int64) (int64, error) {
	const q = `SELECT media_id FROM requests WHERE id = $1`

	var id int64
	err := s.pool.QueryRow(ctx, q, requestID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("request %d: %w", requestID, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("look up media for request %d: %w", requestID, err)
	}
	return id, nil
}

// SaveRequestState writes the process columns and appends an event, together.
//
// The two writes are one transaction because the audit trail is only worth
// having if it describes the state the row is actually in. A state change
// committed without its event leaves a request whose history has a hole in it,
// and that hole is exactly where a user's "why did it do that" lands.
func (s *Store) SaveRequestState(ctx context.Context, st RequestState, ev Event) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := updateStateTx(ctx, tx, st); err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, ev)
	})
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// Event is one audit record.
type Event struct {
	RequestID int64
	Event     string
	FromState State
	ToState   State
	Actor     string
	// Detail is stored as JSON. The release selector's rejection reasons go
	// here, which is what makes "there were results, why did nothing download"
	// answerable after the fact.
	Detail any
}

// RecordEvent appends an audit row.
func (s *Store) RecordEvent(ctx context.Context, ev Event) error {
	return s.recordEvent(ctx, s.pool, ev)
}

func (s *Store) recordEvent(ctx context.Context, q querier, ev Event) error {
	var detail []byte
	if ev.Detail != nil {
		encoded, err := json.Marshal(ev.Detail)
		if err != nil {
			// A detail that will not marshal must not lose the event: the
			// event is the record that something happened, and the detail is
			// commentary on it.
			encoded = []byte(`{"marshal_error":` + strconv.Quote(err.Error()) + `}`)
		}
		detail = encoded
	}

	const sql = `
		INSERT INTO request_events (request_id, event, from_state, to_state, actor, detail)
		VALUES ($1, $2, $3, $4, $5, $6)`

	_, err := q.Exec(ctx, sql,
		ev.RequestID, ev.Event,
		nullState(ev.FromState), nullState(ev.ToState), ev.Actor, detail)
	if err != nil {
		return fmt.Errorf("record event for request %d: %w", ev.RequestID, err)
	}
	return nil
}

// ListEvents returns the audit trail for a request, newest first.
func (s *Store) ListEvents(ctx context.Context, requestID int64, limit int) ([]EventRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	const q = `
		SELECT at, event, COALESCE(from_state, ''), to_state, actor, detail
		  FROM request_events
		 WHERE request_id = $1
		 ORDER BY at DESC, id DESC
		 LIMIT $2`

	rows, err := s.pool.Query(ctx, q, requestID, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []EventRow
	for rows.Next() {
		var (
			row    EventRow
			detail []byte
		)
		if err := rows.Scan(&row.At, &row.Event, &row.FromState, &row.ToState, &row.Actor, &detail); err != nil {
			return nil, err
		}
		row.Detail = detail
		out = append(out, row)
	}
	return out, rows.Err()
}

// EventRow is a stored event.
type EventRow struct {
	At        time.Time
	Event     string
	FromState State
	ToState   State
	Actor     string
	Detail    json.RawMessage
}

// querier is the subset of pgx that both a pool and a transaction satisfy.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

const requestViewColumns = `
	id, media_id, workflow_id, COALESCE(first_run_id, ''), COALESCE(run_id, ''),
	state, attempt_count, error_streak, stall_count, COALESCE(last_error, ''),
	next_run_at, search_deadline, download_deadline,
	COALESCE(selected_title, ''), COALESCE(selected_indexer, ''),
	COALESCE(selected_size, 0), COALESCE(selected_seeders, 0), COALESCE(selected_score, 0),
	COALESCE(qbit_hash, ''), COALESCE(content_path, ''),
	progress, dl_speed, COALESCE(eta_seconds, 0),
	created_at, updated_at, completed_at,
	media_type, tmdb_id, season, title, COALESCE(title_ru, ''),
	COALESCE(original_title, ''), COALESCE(year, 0), COALESCE(poster_path, ''),
	COALESCE(overview, ''), COALESCE(runtime_min, 0),
	in_plex, COALESCE(plex_title, ''), COALESCE(plex_year, 0)`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRequest(row rowScanner) (*Request, error) {
	var (
		r      Request
		m      domain.MediaSpec
		season *int
	)

	err := row.Scan(
		&r.ID, &r.MediaID, &r.WorkflowID, &r.FirstRunID, &r.RunID,
		&r.State, &r.AttemptCount, &r.ErrorStreak, &r.StallCount, &r.LastError,
		&r.NextRunAt, &r.SearchDeadline, &r.DownloadDeadline,
		&r.SelectedTitle, &r.SelectedIndexer,
		&r.SelectedSize, &r.SelectedSeeders, &r.SelectedScore,
		&r.QbitHash, &r.ContentPath,
		&r.Progress, &r.DlSpeed, &r.ETASeconds,
		&r.CreatedAt, &r.UpdatedAt, &r.CompletedAt,
		&m.Ref.Type, &m.Ref.TMDBID, &season, &m.Title, &m.TitleRU,
		&m.OriginalTitle, &m.Year, &r.PosterPath,
		&r.Overview, &m.RuntimeMin,
		&r.InPlex, &r.PlexTitle, &r.PlexYear,
	)
	if err != nil {
		return nil, err
	}

	m.Ref.Season = season
	r.Media = m
	return &r, nil
}

func nullState(s State) *string {
	if s == "" {
		return nil
	}
	v := string(s)
	return &v
}

// upsertMediaTx is UpsertMedia inside an existing transaction.
func upsertMediaTx(ctx context.Context, tx pgx.Tx, spec domain.MediaSpec) (int64, error) {
	const q = `
		INSERT INTO media (
			media_type, tmdb_id, season, title, title_ru, original_title,
			year, runtime_min, released_at, imdb_id, tvdb_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (media_type, tmdb_id, COALESCE(season, -1)) DO UPDATE SET
			title          = EXCLUDED.title,
			title_ru       = COALESCE(EXCLUDED.title_ru, media.title_ru),
			original_title = COALESCE(EXCLUDED.original_title, media.original_title),
			year           = COALESCE(EXCLUDED.year, media.year),
			runtime_min    = COALESCE(EXCLUDED.runtime_min, media.runtime_min),
			released_at    = COALESCE(EXCLUDED.released_at, media.released_at),
			imdb_id        = COALESCE(EXCLUDED.imdb_id, media.imdb_id),
			tvdb_id        = COALESCE(EXCLUDED.tvdb_id, media.tvdb_id),
			updated_at     = now()
		RETURNING id`

	var id int64
	err := tx.QueryRow(ctx, q,
		spec.Ref.Type, spec.Ref.TMDBID, spec.Ref.Season,
		spec.Title, nullString(spec.TitleRU), nullString(spec.OriginalTitle),
		nullInt(spec.Year), nullInt(spec.RuntimeMin), spec.ReleasedAt,
		nullString(spec.IMDBID), nullInt64(spec.TVDBID),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert media %s: %w", spec.Ref, err)
	}
	return id, nil
}

// isUniqueViolation reports whether an error is a unique-constraint breach.
//
// The pgx error is matched on its SQLSTATE rather than on the message: the
// message is localisable and includes the constraint name, which is exactly
// the kind of string that silently stops matching after a schema change.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
