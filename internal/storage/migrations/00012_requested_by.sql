-- +goose Up
-- +goose StatementBegin

-- Who asked for the request.
--
-- The requester was recorded nowhere: the create path writes no event, and
-- the retry path put the name into the event's detail, where the interface
-- showed it as raw JSON. "Кто просил" and "Кто смотрит" are different
-- questions, and only the second had an answer — the watcher list, where the
-- creator is one name among several.
--
-- The column holds the user's id rather than the name, so a request is not
-- attributed to a name that has since been changed, and so that a deleted
-- account leaves a request without an owner rather than with a stale one.
ALTER TABLE requests
    ADD COLUMN IF NOT EXISTS requested_by BIGINT;

-- The projection in request_view was frozen when the view was first created
-- (00001_init.sql), so the new column is invisible to every reader until the
-- view is dropped and re-created.
--
-- DROP + CREATE rather than CREATE OR REPLACE: Postgres refuses the latter
-- when the column list reorders an existing column. DROP + CREATE has no
-- such restriction; the only thing it loses is any GRANTs on the view, and
-- nothing here issues them.
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
       r.requested_by,
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

-- The view is rebuilt without the column and the column is dropped. The
-- requests created since this migration keep the fact of having been asked
-- for; only the name is gone.
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

ALTER TABLE requests
    DROP COLUMN IF EXISTS requested_by;

-- +goose StatementEnd
