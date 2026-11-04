-- +goose Up
-- +goose StatementBegin
ALTER TABLE users ADD COLUMN IF NOT EXISTS must_change_password BOOLEAN NOT NULL DEFAULT FALSE;
-- Only the first application invalidates legacy codes of unknown purpose.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_schema = current_schema() AND table_name = 'invite_codes' AND column_name = 'purpose') THEN
        UPDATE invite_codes SET expires_at = LEAST(expires_at, now()) WHERE used_by IS NULL;
        ALTER TABLE invite_codes ADD COLUMN purpose TEXT NOT NULL DEFAULT 'registration'
            CHECK (purpose IN ('registration', 'telegram_link'));
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE invite_codes DROP COLUMN IF EXISTS purpose;
ALTER TABLE users DROP COLUMN IF EXISTS must_change_password;
