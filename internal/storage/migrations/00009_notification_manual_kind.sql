-- +goose Up
-- +goose StatementBegin

-- Adds the "manual" notification kind to the notifications table.
--
-- The CHECK constraint on notifications.kind (00001_init.sql) lists
-- every legal value the workflow can write. The workflow now writes
-- 'manual' when it enters StateManual and the user is given a
-- candidate list. Same shape as the requests.state update in
-- 00007_request_manual.sql: drop the auto-named constraint and
-- re-add it with the extra value. The IF EXISTS guard makes the
-- migration idempotent under goose's replay behaviour.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check
    CHECK (kind IN (
        'started', 'completed', 'failed', 'skipped', 'canceled', 'manual'
    ));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The reverse path drops 'manual' from the legal kinds. Existing
-- rows in 'manual' state would still satisfy the new constraint
-- (the new CHECK has fewer values, not more), so the revert is
-- safe even on a database that has used the new kind.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check
    CHECK (kind IN (
        'started', 'completed', 'failed', 'skipped', 'canceled'
    ));

-- +goose StatementEnd
