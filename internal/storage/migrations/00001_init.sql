-- +goose Up
-- +goose StatementBegin

-- users comes first: several tables reference it.
CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT UNIQUE,
    -- NULL for users that exist only as a Telegram identity.
    password_hash TEXT,
    is_admin      BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_identities (
    user_id     BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider    TEXT   NOT NULL CHECK (provider IN ('telegram', 'web', 'cli')),
    external_id TEXT   NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, external_id)
);
CREATE INDEX ix_user_identities_user ON user_identities (user_id);

CREATE TABLE sessions (
    token      TEXT PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_sessions_expires ON sessions (expires_at);

CREATE TABLE invite_codes (
    code       TEXT PRIMARY KEY,
    created_by BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_by    BIGINT REFERENCES users (id) ON DELETE SET NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- media is the canonical title, independent of any request for it. Metadata
-- lives here rather than on requests because a second request after a failed
-- one reuses the same row, and because presence in Plex is a property of the
-- media, not of the request that happened to look for it.
CREATE TABLE media (
    id             BIGSERIAL PRIMARY KEY,
    media_type     TEXT    NOT NULL CHECK (media_type IN ('movie', 'tv')),
    tmdb_id        BIGINT  NOT NULL,
    -- NULL for a movie; the season number for a TV request.
    season         SMALLINT,
    title          TEXT    NOT NULL,
    title_ru       TEXT,
    original_title TEXT,
    year           SMALLINT,
    poster_path    TEXT,
    overview       TEXT,
    runtime_min    INTEGER,
    released_at    TIMESTAMPTZ,
    imdb_id        TEXT,
    tvdb_id        BIGINT,

    -- Plex presence. There is no library mirror: we only ever ask about titles
    -- someone requested, and cache the answer here.
    in_plex          BOOLEAN     NOT NULL DEFAULT FALSE,
    plex_rating_key  TEXT,
    plex_section_key TEXT,
    -- What actually matched. Surfaced to the user when a request is skipped,
    -- so a wrong match is visible rather than silently swallowing the request.
    plex_title       TEXT,
    plex_year        SMALLINT,
    plex_added_at    TIMESTAMPTZ,
    plex_checked_at  TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- COALESCE because a plain UNIQUE treats NULLs as distinct, which would let a
-- movie be inserted repeatedly.
CREATE UNIQUE INDEX ux_media_identity ON media (media_type, tmdb_id, COALESCE(season, -1));
CREATE INDEX ix_media_plex ON media (media_type, tmdb_id) WHERE in_plex;

CREATE TABLE requests (
    id       BIGSERIAL PRIMARY KEY,
    media_id BIGINT NOT NULL REFERENCES media (id) ON DELETE CASCADE,

    workflow_id  TEXT NOT NULL,
    -- Stable across ContinueAsNew, unlike run_id. This is the business
    -- identity of the request.
    first_run_id TEXT,
    run_id       TEXT,

    state TEXT NOT NULL CHECK (state IN (
        'pending', 'searching', 'adding', 'downloading',
        'completed', 'failed', 'canceled', 'skipped')),

    -- Number of searches that actually ran and found nothing acceptable.
    -- Deliberately unbounded: over two years any fixed limit is reached and
    -- becomes the real deadline. search.backoff_ladder bounds the rate.
    attempt_count INTEGER NOT NULL DEFAULT 0,
    -- Consecutive indexer failures. Kept apart from attempt_count because an
    -- indexer outage is our problem, not a reason to give up on the media.
    error_streak  INTEGER NOT NULL DEFAULT 0,
    stall_count   INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,

    next_run_at       TIMESTAMPTZ,
    search_deadline   TIMESTAMPTZ NOT NULL,
    download_deadline TIMESTAMPTZ,

    selected_guid     TEXT,
    selected_infohash TEXT,
    selected_title    TEXT,
    selected_indexer  TEXT,
    selected_size     BIGINT,
    selected_seeders  INTEGER,
    selected_score    REAL,
    selected_at       TIMESTAMPTZ,

    -- The authoritative hash as reported by qBittorrent. torrents/add returns
    -- no hash, and for hybrid v1/v2 torrents the client's infohash can differ
    -- from the one in the Torznab metadata.
    qbit_hash    TEXT,
    content_path TEXT,

    -- Episode count of this season in Plex before the download. Plex attaches
    -- the tmdb:// guid to the show, not the season, so "the season arrived"
    -- cannot be checked by guid — it is checked by this count increasing.
    plex_baseline_episodes INTEGER,

    progress    REAL   NOT NULL DEFAULT 0,
    dl_speed    BIGINT NOT NULL DEFAULT 0,
    eta_seconds BIGINT,
    -- When the download last stopped making progress; NULL while it is moving.
    --
    -- It is set for a torrent whose progress is at zero and for one the client
    -- has stalled or stopped, because the question the download loop asks is
    -- "is this moving", not "is this at zero". A stopped torrent at 98% is not
    -- moving either.
    zero_since  TIMESTAMPTZ,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

-- The deduplication arbiter. One active request per unit of media; terminal
-- states fall out of the index, so re-requesting after a failure works.
CREATE UNIQUE INDEX ux_requests_active ON requests (media_id)
    WHERE state IN ('pending', 'searching', 'adding', 'downloading');
CREATE INDEX ix_requests_state ON requests (state, updated_at DESC);
CREATE INDEX ix_requests_list ON requests (created_at DESC);
CREATE INDEX ix_requests_downloading ON requests (qbit_hash) WHERE state = 'downloading';
CREATE INDEX ix_requests_workflow ON requests (workflow_id);

CREATE TABLE request_watchers (
    request_id  BIGINT NOT NULL REFERENCES requests (id) ON DELETE CASCADE,
    user_id     BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Bitmask: 1 = started, 2 = completed, 4 = failed.
    notify_mask SMALLINT    NOT NULL DEFAULT 7,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id, user_id)
);
CREATE INDEX ix_request_watchers_user ON request_watchers (user_id);

CREATE TABLE request_events (
    id         BIGSERIAL PRIMARY KEY,
    request_id BIGINT      NOT NULL REFERENCES requests (id) ON DELETE CASCADE,
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    event      TEXT        NOT NULL,
    from_state TEXT,
    to_state   TEXT,
    actor      TEXT        NOT NULL CHECK (actor IN (
        'api', 'telegram', 'cli', 'workflow', 'scheduler', 'activity', 'reconciler')),
    -- Rejection reasons from the release selector live here. This is what
    -- makes "why didn't it grab anything?" answerable without reproducing it.
    detail JSONB
);
CREATE INDEX ix_request_events_request ON request_events (request_id, at DESC);

CREATE TABLE notifications (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    request_id BIGINT REFERENCES requests (id) ON DELETE CASCADE,
    kind       TEXT   NOT NULL CHECK (kind IN (
        'started', 'completed', 'failed', 'skipped', 'canceled')),
    title      TEXT   NOT NULL,
    body       TEXT,
    payload    JSONB   NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at    TIMESTAMPTZ,
    delivered_telegram_at TIMESTAMPTZ
);
CREATE INDEX ix_notifications_unread ON notifications (user_id, created_at DESC)
    WHERE read_at IS NULL;

-- Releases that must not be picked again: stalled at 0%, died mid-download,
-- or errored. Global rather than per-request, because a dead torrent is dead
-- for everyone.
CREATE TABLE release_blacklist (
    infohash   TEXT PRIMARY KEY,
    reason     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Short opaque id for Telegram inline buttons: callback_data is capped at 64
-- bytes, and a search result page does not fit. Kept in the database rather
-- than in memory so a bot restart does not invalidate buttons already sent.
CREATE TABLE search_sessions (
    id         TEXT PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       TEXT        NOT NULL CHECK (kind IN ('results', 'seasons')),
    payload    JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX ix_search_sessions_expires ON search_sessions (expires_at);

-- The read model the UI and CLI list from. Nothing on a render path talks to
-- Temporal: SQL-backed visibility has no ORDER BY at all, is eventually
-- consistent, and queries require a live worker.
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
DROP VIEW IF EXISTS request_view;
DROP TABLE IF EXISTS search_sessions;
DROP TABLE IF EXISTS release_blacklist;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS request_events;
DROP TABLE IF EXISTS request_watchers;
DROP TABLE IF EXISTS requests;
DROP TABLE IF EXISTS media;
DROP TABLE IF EXISTS invite_codes;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS user_identities;
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
