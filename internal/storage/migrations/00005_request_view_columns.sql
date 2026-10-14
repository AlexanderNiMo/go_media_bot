-- +goose Up
-- +goose StatementBegin

-- The projection in request_view was frozen when the view was first created
-- (00001_init.sql). Adding columns to the underlying table does not change
-- the view, so the new fields added by 00004_manual_release.sql are invisible
-- to every reader. CREATE OR REPLACE VIEW pulls them in.

CREATE OR REPLACE VIEW request_view AS
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
-- The new view is the one in 00001_init.sql; restoring it would mean
-- re-listing every column by hand and forgetting none, so this is a no-op.
-- A real rollback is: drop the new columns from requests (00004) and the
-- old shape comes back through the unchanged view.
SELECT 1;
-- +goose StatementEnd
