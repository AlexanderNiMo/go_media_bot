package storage

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Candidate is one release the user can pick from when the selector said
// "nothing acceptable" but the indexer did return results.
//
// Rank is 1-based, in selector-score order: the same order the web UI
// and Telegram show. Magnet is the URL the picker hands to the torrent
// client; the fallback to a .torrent URL is the torrent client's job.
//
// RejectionReason is the human-readable form of the selector's reason
// code ("russian_audio: missing", "size_under: 0.4 GB < min 1.5 GB",
// etc.) so the user picks with context, not blind.
//
// PickedAt is the moment the user committed to this candidate. Nil on
// rows the user has not touched, set on the row the user picked. The
// workflow polls and uses the first row with PickedAt set as the
// manual-pick choice; a stale PickedAt that the workflow has already
// consumed is gone with the rest of the list when the workflow clears
// the candidates after a successful pick or a 14-day timeout.
type Candidate struct {
	Rank    int
	Title   string
	Indexer string
	// ThreadID is the forum thread the release came from. It is what the
	// season watcher matches on: a tracker replaces the torrent in the
	// same thread when the rest of a season is published, so a pack the
	// user picked by hand can be followed the same way an automatic pick
	// is.
	ThreadID        string
	InfoHash        string
	MagnetLink      string
	SourceURL       string
	SizeBytes       int64
	Seeders         int
	Leechers        int
	RejectionReason string
	PickedAt        *time.Time
}

// StoreCandidates replaces the candidate list for a request.
//
// The list is overwritten on every fruitless search: a fresh search may
// have surfaced a different set, and presenting the most recent one is
// what the user expects when they sit down to pick. The previous list is
// gone, the events on the request tell the user which search each list
// came from.
//
// PickedAt is left NULL on every row — it is the user's mark, not the
// loader's.
//
// A pick the user has already made outranks a fresher list. The marked
// row is one of the rows the DELETE below removes, so without the guard
// the choice would disappear with it — and the choice is the one thing on
// this table that the user did on purpose. The workflow consumes a pick
// long before the next search, so the guard only matters when the
// wake-up signal did not arrive and the request is still asleep when the
// next search runs.
func (s *Store) StoreCandidates(ctx context.Context, requestID int64, candidates []Candidate) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var picked bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM request_candidates
			WHERE request_id = $1 AND picked_at IS NOT NULL
		)
	`, requestID).Scan(&picked); err != nil {
		return fmt.Errorf("check for a picked candidate: %w", err)
	}
	if picked {
		return tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM request_candidates WHERE request_id = $1`, requestID,
	); err != nil {
		return fmt.Errorf("clear old candidates: %w", err)
	}

	for _, c := range candidates {
		if _, err := tx.Exec(ctx, `
			INSERT INTO request_candidates
				(request_id, rank, title, indexer, thread_id, info_hash,
				 magnet_link, source_url, size_bytes, seeders, leechers,
				 rejection_reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, requestID, c.Rank, c.Title, c.Indexer, nullString(c.ThreadID),
			c.InfoHash, c.MagnetLink, c.SourceURL, c.SizeBytes, c.Seeders,
			c.Leechers, c.RejectionReason); err != nil {
			return fmt.Errorf("insert candidate rank %d: %w", c.Rank, err)
		}
	}

	return tx.Commit(ctx)
}

// ListCandidates returns the candidates stored for a request, in rank order.
//
// Returns an empty slice (not nil, not an error) when the request has no
// candidate list, so callers can render an "empty list" UI without a
// special case.
func (s *Store) ListCandidates(ctx context.Context, requestID int64) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT rank, title, indexer, COALESCE(thread_id, ''), info_hash,
		       magnet_link, source_url, size_bytes, seeders, leechers,
		       rejection_reason, picked_at
		FROM request_candidates
		WHERE request_id = $1
		ORDER BY rank ASC
	`, requestID)
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(
			&c.Rank, &c.Title, &c.Indexer, &c.ThreadID, &c.InfoHash,
			&c.MagnetLink, &c.SourceURL, &c.SizeBytes, &c.Seeders, &c.Leechers,
			&c.RejectionReason, &c.PickedAt,
		); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	return out, nil
}

// CountCandidates returns how many candidates a request has on offer.
//
// The workflow asks this question once per search — "is there anything
// for the user to pick from?" — and the answer decides whether the
// request is worth offering a manual list at all. Counting is cheaper
// than reading the rows, and the count is all the workflow needs to make
// that decision.
func (s *Store) CountCandidates(ctx context.Context, requestID int64) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM request_candidates WHERE request_id = $1`, requestID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count candidates: %w", err)
	}
	return n, nil
}

// MarkPicked sets picked_at on the chosen candidate and returns the row.
//
// Idempotent: a second call for the same rank overwrites the first
// timestamp. Returns ErrNoSuchCandidate when the rank does not exist.
//
// Atomic at the row level. The partial index on picked_at makes the
// workflow's subsequent "is there a pick?" poll cheap.
func (s *Store) MarkPicked(ctx context.Context, requestID int64, rank int) (Candidate, error) {
	var c Candidate
	err := s.pool.QueryRow(ctx, `
		UPDATE request_candidates
		SET picked_at = NOW()
		WHERE request_id = $1 AND rank = $2
		RETURNING rank, title, indexer, COALESCE(thread_id, ''), info_hash,
		          magnet_link, source_url, size_bytes, seeders, leechers,
		          rejection_reason, picked_at
	`, requestID, rank).Scan(
		&c.Rank, &c.Title, &c.Indexer, &c.ThreadID, &c.InfoHash,
		&c.MagnetLink, &c.SourceURL, &c.SizeBytes, &c.Seeders, &c.Leechers,
		&c.RejectionReason, &c.PickedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return Candidate{}, ErrNoSuchCandidate
		}
		return Candidate{}, fmt.Errorf("mark candidate picked: %w", err)
	}
	return c, nil
}

// GetPickedCandidate returns the candidate the user picked, or
// ErrNoSuchCandidate if no row has picked_at set.
//
// The workflow polls this on every tick of the manual-mode loop. The
// partial index on picked_at keeps the poll at index-scan cost; the
// alternative (list all candidates and check picked_at in Go) would
// be the same correctness, just a different read path.
//
// Only one candidate should ever be picked (the table is one row per
// rank, and we ask for the first one with picked_at set). The
// function returns one row; the caller treats that as the pick.
func (s *Store) GetPickedCandidate(ctx context.Context, requestID int64) (Candidate, error) {
	var c Candidate
	err := s.pool.QueryRow(ctx, `
		SELECT rank, title, indexer, COALESCE(thread_id, ''), info_hash,
		       magnet_link, source_url, size_bytes, seeders, leechers,
		       rejection_reason, picked_at
		FROM request_candidates
		WHERE request_id = $1 AND picked_at IS NOT NULL
		ORDER BY picked_at ASC
		LIMIT 1
	`, requestID).Scan(
		&c.Rank, &c.Title, &c.Indexer, &c.ThreadID, &c.InfoHash,
		&c.MagnetLink, &c.SourceURL, &c.SizeBytes, &c.Seeders, &c.Leechers,
		&c.RejectionReason, &c.PickedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return Candidate{}, ErrNoSuchCandidate
		}
		return Candidate{}, fmt.Errorf("get picked candidate: %w", err)
	}
	return c, nil
}

// ClearCandidates removes any candidate list for a request.
//
// Called on cancel, on transition out of StateManual back to StatePending
// (timeout), and after the workflow has consumed a picked candidate —
// in all of those the previous list is stale and would mislead the user
// if it lingered.
func (s *Store) ClearCandidates(ctx context.Context, requestID int64) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM request_candidates WHERE request_id = $1`, requestID,
	)
	if err != nil {
		return fmt.Errorf("clear candidates: %w", err)
	}
	return nil
}

// ErrNoSuchCandidate is returned when a candidate row does not exist for
// the given request+rank. Typed so the web handler can map it to a 404
// without sniffing error strings.
var ErrNoSuchCandidate = errors.New("storage: no such candidate")

// isNoRows is a small helper for the pgx "no rows" sentinel. Lives next
// to the candidate code so a future change to the driver (sqlx, stdlib,
// etc.) only touches this file.
func isNoRows(err error) bool {
	return err != nil && err.Error() == "no rows in result set"
}
