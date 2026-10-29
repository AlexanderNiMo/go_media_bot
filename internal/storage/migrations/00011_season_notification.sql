-- +goose Up
-- +goose StatementBegin

-- Adds the "season" notification kind and lets a request that waits for the
-- rest of a running season hold on to its media.
--
-- Two things happen here.
--
-- The first is the notification kind. The workflow now writes 'season' when
-- a request starts waiting for the rest of a season that is still being
-- released, and when that wait ends without the season being completed. Same
-- shape as the 'manual' kind in 00009: the CHECK constraint on
-- notifications.kind lists every legal value, so it is dropped and re-added
-- with the extra one. The IF EXISTS guard makes the migration idempotent
-- under goose's replay behaviour.
--
-- The second is the notify_mask. The mask has five bits now — started,
-- completed, failed, manual and season — and the default of 15 was "every
-- bit that existed". A watcher with 15 has not opted out of anything: there
-- has never been a way to opt out, and 15 is what the schema defaulted to.
-- Bumping those rows to 31 is consistent with what they would have been set
-- to if AddWatcher had been inserting 31 from the start. Watchers with any
-- other mask are left alone.
--
-- The third is the deduplication index. A request in the 'ongoing' state is
-- waiting for the rest of a season, which can take weeks, and until now it
-- fell out of the index — so the same season could be requested again while
-- the first request was still waiting for it. The index is dropped and
-- re-created with 'ongoing' added; the states are listed explicitly rather
-- than derived, so the predicate stays indexable.

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check
    CHECK (kind IN (
        'started', 'completed', 'failed', 'skipped', 'canceled', 'manual', 'season'
    ));

-- The forum thread a candidate came from. A release that the user picked by
-- hand is a pack like any other, and the season watcher recognises the
-- update of a pack by its thread: without this column a hand-picked pack
-- could never be followed, and the request would wait out the season for an
-- update it is unable to recognise.
ALTER TABLE request_candidates
    ADD COLUMN IF NOT EXISTS thread_id TEXT;

ALTER TABLE request_watchers
    ALTER COLUMN notify_mask SET DEFAULT 31;

UPDATE request_watchers
SET notify_mask = 31
WHERE notify_mask = 15;

ALTER TABLE users
    ALTER COLUMN telegram_notify_mask SET DEFAULT 31;

UPDATE users
SET telegram_notify_mask = 31
WHERE telegram_notify_mask = 15;

-- The third thing is the deduplication index. A request in the 'ongoing'
-- state is waiting for the rest of a season, which can take weeks, and until
-- now it fell out of the index — so the same season could be requested again
-- while the first request was still waiting for it. The index is dropped and
-- re-created with 'ongoing' added; the states are listed explicitly rather
-- than derived, so the predicate stays indexable.
--
-- The duplicates that the old behaviour allowed have to go first. Since
-- 00010 a request could sit in the 'ongoing' state while a second row for
-- the same season was created, and a unique index over both states refuses
-- to exist while they are there — a migration that fails is a deployment
-- that does not start.
--
-- The oldest row keeps its state; the others are marked failed with a note.
-- A request created while another one was already watching the same season
-- was never going to be worked on.
UPDATE requests r
   SET state = 'failed',
       last_error = 'a request for this season was already running',
       completed_at = COALESCE(r.completed_at, now())
  FROM requests keep
 WHERE keep.media_id = r.media_id
   AND keep.id < r.id
   AND r.state IN ('pending', 'searching', 'adding', 'downloading', 'ongoing')
   AND keep.state IN ('pending', 'searching', 'adding', 'downloading', 'ongoing');

DROP INDEX IF EXISTS ux_requests_active;
CREATE UNIQUE INDEX ux_requests_active ON requests (media_id)
    WHERE state IN ('pending', 'searching', 'adding', 'downloading', 'ongoing');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The reverse path. The UPDATE on existing rows is not reverted: down-migrating
-- would silently re-disable the season notifications for every watcher, which
-- is the exact regression this migration is preventing. Leaving the rows at 31
-- is the safer default for a manual rollback.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check
    CHECK (kind IN (
        'started', 'completed', 'failed', 'skipped', 'canceled', 'manual'
    ));

ALTER TABLE request_watchers
    ALTER COLUMN notify_mask SET DEFAULT 15;

ALTER TABLE users
    ALTER COLUMN telegram_notify_mask SET DEFAULT 15;

DROP INDEX IF EXISTS ux_requests_active;
CREATE UNIQUE INDEX ux_requests_active ON requests (media_id)
    WHERE state IN ('pending', 'searching', 'adding', 'downloading');

-- +goose StatementEnd
