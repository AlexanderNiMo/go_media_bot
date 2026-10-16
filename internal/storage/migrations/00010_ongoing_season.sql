-- +goose Up
-- +goose StatementBegin

-- Adds the "ongoing" state and the per-selection columns the
-- ongoing-season branch reads.
--
-- StateOngoing is the half-season case: a torrent was added
-- but the file count is below the season's episode count, so the
-- workflow is watching for a release from the same forum thread
-- that bundles the remaining episodes.
--
-- The CHECK constraint is dropped and re-added with the new
-- value, the same idempotent pattern that 00007 and 00009 used
-- for StateManual and NotificationManual. The IF EXISTS guard
-- makes the migration safe to re-run.
ALTER TABLE requests DROP CONSTRAINT IF EXISTS requests_state_check;
ALTER TABLE requests ADD CONSTRAINT requests_state_check
    CHECK (state IN (
        'pending', 'searching', 'adding', 'downloading',
        'completed', 'failed', 'canceled', 'skipped',
        'manual', 'ongoing'
    ));

-- SelectedThreadID is set by the indexer path: it is the forum
-- thread id captured from the torrent description, prefixed with
-- the indexer name when the match came from a per-tracker
-- pattern. The workflow reads it on the next search tick to
-- decide whether a new release is an update of an ongoing
-- season rather than a fresh release.
--
-- Nullable rather than NOT NULL DEFAULT '': the existing
-- selected_title et al. are nullable, and the request_view
-- projection uses COALESCE(..., '') to keep the empty-string
-- default at the read boundary. The matching helper in
-- internal/storage/store.go is nullString, which returns nil for
-- "" and a non-nil pointer otherwise; an UPDATE through that
-- helper needs a nullable column on the other end.
ALTER TABLE requests
    ADD COLUMN IF NOT EXISTS selected_thread_id TEXT;

-- SelectedVideoCount and SelectedEpisodeCount are populated
-- when the torrent is added. Together they drive the
-- full-or-ongoing decision in the workflow.
ALTER TABLE requests
    ADD COLUMN IF NOT EXISTS selected_video_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE requests
    ADD COLUMN IF NOT EXISTS selected_episode_count INTEGER NOT NULL DEFAULT 0;

-- The view in 00005 does not know about the new columns. Re-create
-- it with the new fields appended at the end of the SELECT list,
-- matching the rule in mediad-migrations that reordering is the
-- surest way to break row.Scan. The reader-side `requestViewColumns`
-- constant and the `scanRequest` row.Scan in internal/storage/requests.go
-- pick the new columns up at the same time, in the same commit.
DROP VIEW IF EXISTS request_view;
CREATE VIEW request_view AS
SELECT r.id,
       r.media_id,
       r.workflow_id,
       r.first_run_id,
       r.run_id,
       r.state,
       r.attempt_count,
       r.error_streak,
       r.stall_count,
       r.last_error,
       r.next_run_at,
       r.search_deadline,
       r.download_deadline,
       r.selected_title,
       r.selected_indexer,
       r.selected_size,
       r.selected_seeders,
       r.selected_score,
       r.selected_link,
       r.selected_infohash,
       r.qbit_hash,
       r.content_path,
       r.progress,
       r.dl_speed,
       r.eta_seconds,
       r.created_at,
       r.updated_at,
       r.completed_at,
       r.selected_thread_id,
       r.selected_video_count,
       r.selected_episode_count,
       m.media_type,
       m.tmdb_id,
       m.season,
       m.title,
       m.title_ru,
       m.original_title,
       m.year,
       m.poster_path,
       m.overview,
       m.runtime_min,
       m.in_plex,
       m.plex_title,
       m.plex_year
FROM requests r
JOIN media m ON m.id = r.media_id;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Real rollback is a forward migration; this is a no-op.
SELECT 1;
-- +goose StatementEnd
