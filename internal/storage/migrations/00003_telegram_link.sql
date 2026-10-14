-- +goose Up
-- +goose StatementBegin

-- The notification preferences for the Telegram channel. Per-user, not per
-- request, because "do I want a Telegram ping when a download finishes" is
-- a property of the user, not of the particular request that is finishing.
--
-- Stored as a small bitmask rather than four booleans so the storage layer
-- can hand the value back in one column and the worker can decide in one
-- comparison. The four bits line up with NotificationKind: started=1,
-- completed=2, failed=4, skipped=8. A user who has not configured anything
-- yet has all four set, which is what they almost certainly want.
ALTER TABLE users
    ADD COLUMN telegram_notify_mask SMALLINT NOT NULL DEFAULT 15;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users DROP COLUMN IF EXISTS telegram_notify_mask;
-- +goose StatementEnd
