-- +goose Up
-- +goose StatementBegin

-- Adds the "manual" state to the request lifecycle, for the case where the
-- indexer returned results but none passed the selector, and the user is
-- given a list of candidates to pick from. Same shape as every other state
-- transition: an enum value plus the rows that the new phase reads/writes.
--
-- The CHECK constraint on requests.state (00001_init.sql) lists every legal
-- value. Adding 'manual' here is a two-step on Postgres: the auto-generated
-- constraint name (requests_state_check) is dropped and re-added with the
-- extra value. The IF EXISTS guard makes the migration idempotent — a re-run
-- on a database that already saw the change is a no-op for both halves of
-- the pair, and the resulting constraint has the same name and definition
-- either way.
ALTER TABLE requests DROP CONSTRAINT IF EXISTS requests_state_check;
ALTER TABLE requests ADD CONSTRAINT requests_state_check
    CHECK (state IN (
        'pending', 'searching', 'adding', 'downloading',
        'completed', 'failed', 'canceled', 'skipped', 'manual'
    ));

-- Storage for the candidate list the user is picking from.
--
-- One row per (request, rank) pair. Rank is the position the selector
-- assigned the release in the most recent search — used to present the
-- list in score order, and as the user's handle ("/pick 3") on Telegram.
--
-- We store the full release data (link, info_hash, size, seeders) rather
-- than just the infohash, because the user has to be able to read the title
-- and the size to make a pick, and the workflow does not have the original
-- `domain.Release` after a ContinueAsNew.
--
-- Cleared on pick, on cancel, and when the request re-enters search.
CREATE TABLE IF NOT EXISTS request_candidates (
    request_id BIGINT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,

    -- Rank is 1-based, in selector score order. PRIMARY KEY guarantees no
    -- two candidates share a rank for the same request, which is what
    -- /pick <rank> keys on.
    rank        INT     NOT NULL,

    -- The release data the user needs to read the row in the web UI or on
    -- Telegram. Title is shown, magnet_link is what the picker consumes.
    title       TEXT    NOT NULL,
    indexer     TEXT    NOT NULL,
    info_hash   TEXT    NOT NULL,
    magnet_link TEXT    NOT NULL,
    source_url  TEXT    NOT NULL DEFAULT '',
    size_bytes  BIGINT  NOT NULL,
    seeders     INT     NOT NULL,
    leechers    INT     NOT NULL DEFAULT 0,

    -- Selector's reason for refusing: "russian_audio: missing", "size_under",
    -- etc. Shown to the user, so the pick is informed, not blind.
    rejection_reason TEXT NOT NULL,

    -- Non-null once the user has picked this candidate. The workflow
    -- polls candidates and treats the first one with a non-null picked_at
    -- as the user's choice. Cleared (along with the rest of the list) on
    -- consume or on the 14-day manual-mode timeout.
    picked_at    TIMESTAMPTZ,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (request_id, rank)
);

-- A partial index over just the picked rows keeps the workflow's poll cheap:
-- the question it asks on every tick is "is there a picked candidate?",
-- and a partial index makes that an index-only scan, not a sequential one
-- over the whole request_candidates table.
CREATE INDEX IF NOT EXISTS ix_request_candidates_picked
    ON request_candidates (request_id)
    WHERE picked_at IS NOT NULL;

-- Lookups by request_id for the list view. The PRIMARY KEY already covers
-- (request_id, rank), so a secondary index on request_id alone is what
-- the list query needs.
CREATE INDEX IF NOT EXISTS ix_request_candidates_request
    ON request_candidates (request_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The reverse path is rare (down-migrations on production are unusual)
-- but straightforward. The candidates table is dropped first because the
-- constraint we are about to revert is the only thing that could reference
-- a 'manual' state, and there is no row in `requests` in 'manual' state
-- while the constraint still says 'manual' is legal.
DROP TABLE IF EXISTS request_candidates;
ALTER TABLE requests DROP CONSTRAINT IF EXISTS requests_state_check;
ALTER TABLE requests ADD CONSTRAINT requests_state_check
    CHECK (state IN (
        'pending', 'searching', 'adding', 'downloading',
        'completed', 'failed', 'canceled', 'skipped'
    ));

-- +goose StatementEnd
