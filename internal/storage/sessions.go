package storage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Search sessions are the short handles behind inline buttons.
//
// Telegram caps callback_data at 64 bytes, and a page of search results does
// not fit: a TMDB id, a title and a season list run past it immediately, and a
// title in Russian is two bytes per character. So the button carries an opaque
// id and the payload lives here.
//
// In the database rather than in memory, and that is the point: a bot restart
// would otherwise invalidate every button in every chat, and the user's
// experience of that is pressing a button and being told the search expired.

// SearchSessionTTL is how long a set of results stays selectable.
//
// Long enough that somebody can come back to it after looking something up,
// short enough that the table does not accumulate.
const SearchSessionTTL = time.Hour

// SearchSessionKind is what a session holds.
type SearchSessionKind string

const (
	// SessionResults holds search results awaiting a choice.
	SessionResults SearchSessionKind = "results"
	// SessionSeasons holds the seasons of a series awaiting a choice.
	SessionSeasons SearchSessionKind = "seasons"
)

// SearchSession is one stored payload.
type SearchSession struct {
	ID        string
	UserID    int64
	Kind      SearchSessionKind
	Payload   json.RawMessage
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ErrSessionExpired is returned for a session that has aged out.
//
// Distinct from ErrNotFound because the bot says something different: one is
// "that button is from an old conversation", the other is "that button is not
// mine".
var ErrSessionExpired = errors.New("the search session has expired")

// SaveSearchSession stores a payload and returns its handle.
//
// The payload is marshalled here rather than by the caller so that what goes
// into the column is always valid JSON — a session whose payload cannot be
// read back is a button that fails when pressed, which is the worst moment to
// find out.
func (s *Store) SaveSearchSession(
	ctx context.Context, userID int64, kind SearchSessionKind, payload any,
) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode the search session: %w", err)
	}

	id, err := randomID(12)
	if err != nil {
		return "", err
	}

	const q = `
		INSERT INTO search_sessions (id, user_id, kind, payload, expires_at)
		VALUES ($1, $2, $3, $4, $5)`

	if _, err := s.pool.Exec(ctx, q, id, userID, kind, raw,
		time.Now().Add(SearchSessionTTL)); err != nil {
		return "", fmt.Errorf("save the search session: %w", err)
	}
	return id, nil
}

// GetSearchSession loads a payload for a user.
//
// The user id is part of the lookup, not checked afterwards. A callback id
// travels through Telegram and comes back in a button press, and without this
// somebody could act on a session that is not theirs by quoting an id — which
// for a seasons session would let them start a request under another account.
func (s *Store) GetSearchSession(
	ctx context.Context, id string, userID int64, into any,
) (*SearchSession, error) {
	const q = `
		SELECT id, user_id, kind, payload, created_at, expires_at
		  FROM search_sessions
		 WHERE id = $1 AND user_id = $2`

	var session SearchSession
	err := s.pool.QueryRow(ctx, q, id, userID).Scan(
		&session.ID, &session.UserID, &session.Kind, &session.Payload,
		&session.CreatedAt, &session.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("search session %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get the search session: %w", err)
	}

	if time.Now().After(session.ExpiresAt) {
		return nil, fmt.Errorf("search session %s: %w", id, ErrSessionExpired)
	}

	if into != nil {
		if err := json.Unmarshal(session.Payload, into); err != nil {
			return nil, fmt.Errorf("decode the search session: %w", err)
		}
	}
	return &session, nil
}

// DeleteSearchSession removes a session that has been used.
//
// Called once a choice has been made, so that a button pressed twice does not
// start the same request twice from the same menu.
func (s *Store) DeleteSearchSession(ctx context.Context, id string, userID int64) error {
	const q = `DELETE FROM search_sessions WHERE id = $1 AND user_id = $2`
	if _, err := s.pool.Exec(ctx, q, id, userID); err != nil {
		return fmt.Errorf("delete the search session: %w", err)
	}
	return nil
}

// DeleteExpiredSearchSessions clears sessions that have lapsed.
func (s *Store) DeleteExpiredSearchSessions(ctx context.Context) (int64, error) {
	const q = `DELETE FROM search_sessions WHERE expires_at < now()`
	tag, err := s.pool.Exec(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("delete expired search sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// randomID returns a short URL-safe identifier.
//
// The alphabet is base64url with no padding, so an id is safe inside
// callback_data and inside a URL: Telegram's own constraints on the field are
// about length, but a value that needed escaping would be a bug waiting for
// the first unusual byte.
func randomID(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate an identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
