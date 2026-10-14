-- +goose Up
-- +goose StatementBegin

-- A link submitted by the user through the web UI, as a fallback when the
-- indexer returned nothing useful. The full magnet goes in selected_link and
-- the infohash in selected_infohash, populated after qBittorrent accepts
-- the torrent. Either may be empty during the brief window between submit
-- and accept.
--
-- IF NOT EXISTS so the migration is idempotent: goose's NewProvider.Up
-- replays already-applied statements under some configurations, and a
-- hard ADD would crash the worker on every restart in that case.
ALTER TABLE requests
    ADD COLUMN IF NOT EXISTS selected_link      TEXT,
    ADD COLUMN IF NOT EXISTS selected_infohash  TEXT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE requests
    DROP COLUMN IF EXISTS selected_link,
    DROP COLUMN IF EXISTS selected_infohash;
-- +goose StatementEnd
