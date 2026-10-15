-- +goose Up
-- +goose StatementBegin

-- Wires the manual-mode notification through the watcher notify_mask.
--
-- The mask has four bits that gate whether a notification ROW is created at
-- all in CreateNotifications:
--   bit 1: started
--   bit 2: completed
--   bit 4: failed
--   bit 8: manual   (workflow entered StateManual; user has a list to pick from)
--
-- The schema default was 7, which is "started + completed + failed". That
-- was the right default when manual did not exist; it became a silent
-- bug once manual did, because a brand-new watcher with the default mask
-- never gets the manual notification at all. The fix is two parts:
--
--   1. The schema default is now 15 (all four bits). New watchers opt in to
--      every kind by default. There is no UI to flip individual bits today;
--      adding one is a separate, additive change.
--
--   2. Existing rows are bumped from 7 to 15 in the same migration. The
--      assumption is that a watcher with notify_mask = 7 has not opted out
--      of anything — there has never been a way to opt out, and 7 is what
--      the schema defaulted to. Bumping is consistent with what those
--      watchers would have been set to if AddWatcher had been inserting 15
--      from the start. Watchers that already have a different mask (none
--      today) are left alone.

ALTER TABLE request_watchers
    ALTER COLUMN notify_mask SET DEFAULT 15;

UPDATE request_watchers
SET notify_mask = 15
WHERE notify_mask = 7;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The DOWN path is rare (down-migrations on production are unusual), and the
-- only thing the DOWN restores is the schema default. The UPDATE on existing
-- rows is not reverted: down-migrating would silently re-disable manual
-- notifications for every watcher, which is the exact regression this
-- migration is preventing. Leaving the rows at 15 is the safer default for
-- a manual rollback.
ALTER TABLE request_watchers
    ALTER COLUMN notify_mask SET DEFAULT 7;

-- +goose StatementEnd
