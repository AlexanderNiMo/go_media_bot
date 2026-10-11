-- +goose Up
-- +goose StatementBegin

-- The download watcher and the request workflow both write to requests, and
-- this migration is about keeping them out of each other's way.
--
-- The split is by ownership: the workflow owns the process — which phase a
-- request is in, when the next attempt is due, which release was chosen — and
-- the watcher owns the facts it observes from the torrent client: how far
-- along a download is, what the client says about it, and when it stopped
-- moving.
--
-- Before this, UpdateState wrote the progress columns too. That was harmless
-- while the workflow was the only writer, and would have become a silent bug
-- the moment a second one appeared: the workflow never sets progress, so every
-- state change would have reset it to zero, and the interface would have shown
-- a download starting over.

-- What the torrent client reports, verbatim: downloading, stalledDL, stoppedDL,
-- error, missingFiles and so on.
--
-- It is stored rather than interpreted because the interpretation belongs to
-- the request workflow, and because seeing the raw value is what makes a
-- disagreement between this project and the client diagnosable.
ALTER TABLE requests ADD COLUMN qbit_state TEXT;

-- When the torrent stopped making progress, or first went missing from the
-- client. NULL means it is moving, or present.
--
-- It is separate from zero_since because the two answer different questions, and the
-- difference decides whether the release is worth remembering as bad: a
-- torrent that never started is an infrastructure problem and may be retried,
-- while one that got to 40% and vanished means the release itself is dead.
ALTER TABLE requests ADD COLUMN missing_since TIMESTAMPTZ;

-- Downloads the watcher has to poll. It is a partial index on the same
-- condition the query uses, so it stays proportional to the number of active
-- downloads rather than to the size of the table.
CREATE INDEX ix_requests_downloading_hashes ON requests (qbit_hash)
    WHERE state = 'downloading' AND qbit_hash IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS ix_requests_downloading_hashes;
ALTER TABLE requests DROP COLUMN IF EXISTS missing_since;
ALTER TABLE requests DROP COLUMN IF EXISTS qbit_state;
-- +goose StatementEnd
