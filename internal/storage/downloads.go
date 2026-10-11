package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ActiveDownload is one torrent the watcher has to poll.
type ActiveDownload struct {
	RequestID int64
	Hash      string
	// State and the timestamps are what the watcher compares against, so it
	// can write only when something actually changed.
	Progress     float64
	QbitState    string
	ZeroSince    *time.Time
	MissingSince *time.Time
}

// ListActiveDownloads returns every request currently waiting on a torrent.
//
// The watcher calls this on each of its own cycles rather than being handed a
// list, and that is what makes a download added between two cycles get picked
// up within one interval instead of at the next restart.
func (s *Store) ListActiveDownloads(ctx context.Context) ([]ActiveDownload, error) {
	const q = `
		SELECT id, qbit_hash, progress, COALESCE(qbit_state, ''), zero_since, missing_since
		  FROM requests
		 WHERE state = 'downloading' AND qbit_hash IS NOT NULL AND qbit_hash <> ''`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list active downloads: %w", err)
	}
	defer rows.Close()

	var out []ActiveDownload
	for rows.Next() {
		var d ActiveDownload
		if err := rows.Scan(&d.RequestID, &d.Hash, &d.Progress,
			&d.QbitState, &d.ZeroSince, &d.MissingSince); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ActiveDownloadHashes returns just the hashes, for the batched client query.
func (s *Store) ActiveDownloadHashes(ctx context.Context) ([]string, error) {
	const q = `
		SELECT DISTINCT qbit_hash
		  FROM requests
		 WHERE state = 'downloading' AND qbit_hash IS NOT NULL AND qbit_hash <> ''`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list active download hashes: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// DownloadFacts is what the watcher observed about one torrent.
//
// Every field is a fact from the client or a direct consequence of one. None
// of them is a decision: the watcher does not decide that a download failed or
// that a release is bad, it records what it saw and the workflow interprets it.
type DownloadFacts struct {
	Progress     float64
	DlSpeed      int64
	ETASeconds   int64
	QbitState    string
	ContentPath  string
	ZeroSince    *time.Time
	MissingSince *time.Time
}

// UpdateDownloadFacts writes what the watcher observed.
//
// It touches only the columns the watcher owns, which is what lets the two
// writers — this and the workflow's state changes — run against the same row
// without coordinating.
func (s *Store) UpdateDownloadFacts(ctx context.Context, requestID int64, f DownloadFacts) error {
	const q = `
		UPDATE requests SET
			progress      = $2,
			dl_speed      = $3,
			eta_seconds   = $4,
			qbit_state    = $5,
			content_path  = COALESCE(NULLIF($6, ''), content_path),
			zero_since    = $7,
			missing_since = $8,
			updated_at    = now()
		WHERE id = $1`

	_, err := s.pool.Exec(ctx, q,
		requestID, f.Progress, f.DlSpeed, nullInt64(f.ETASeconds),
		nullString(f.QbitState), f.ContentPath, f.ZeroSince, f.MissingSince)
	if err != nil {
		return fmt.Errorf("update download facts for request %d: %w", requestID, err)
	}
	return nil
}

// DownloadProgress is what the request workflow reads on each of its polls.
type DownloadProgress struct {
	State        State
	Progress     float64
	QbitState    string
	ZeroSince    *time.Time
	MissingSince *time.Time
	ContentPath  string
	LastError    string
	QbitHash     string
}

// ReadDownloadProgress returns the watcher's latest observations for a request.
//
// It is one indexed read of a single row, which is the point: the request
// workflow polls every few minutes for up to two days, and going to the
// torrent client on each of those polls would mean a second process competing
// for the same session — and, for a client that bans an IP after repeated
// failed logins, a second thing that can lock everyone out.
func (s *Store) ReadDownloadProgress(ctx context.Context, requestID int64) (DownloadProgress, error) {
	const q = `
		SELECT state, progress, COALESCE(qbit_state, ''), zero_since, missing_since,
		       COALESCE(content_path, ''), COALESCE(last_error, ''), COALESCE(qbit_hash, '')
		  FROM requests
		 WHERE id = $1`

	var p DownloadProgress
	err := s.pool.QueryRow(ctx, q, requestID).Scan(
		&p.State, &p.Progress, &p.QbitState, &p.ZeroSince, &p.MissingSince,
		&p.ContentPath, &p.LastError, &p.QbitHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return DownloadProgress{}, fmt.Errorf("request %d: %w", requestID, ErrNotFound)
	}
	if err != nil {
		return DownloadProgress{}, fmt.Errorf("read download progress for request %d: %w", requestID, err)
	}
	return p, nil
}
