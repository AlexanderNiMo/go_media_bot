package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Watchers
// ---------------------------------------------------------------------------

// Watcher is someone who wants to be told about a request.
//
// Watchers live in Postgres rather than in the workflow, and that is the
// decision that removes a whole class of races: "user B asks for what user A
// is already downloading" becomes one INSERT here, with no signal to the
// running workflow at all. The workflow only ever learns the list by asking
// the NotifyWatchers activity to read it.
type Watcher struct {
	UserID     int64
	Username   string
	TelegramID string
	NotifyMask int
}

// Watcher notification bits.
const (
	NotifyStarted   = 1
	NotifyCompleted = 2
	NotifyFailed    = 4
)

// AddWatcher attaches a user to a request.
//
// Idempotent: the primary key is (request_id, user_id), so asking twice is a
// no-op rather than an error. A user re-asking for something they already
// watch is a normal thing to do.
func (s *Store) AddWatcher(ctx context.Context, requestID, userID int64) error {
	const q = `
		INSERT INTO request_watchers (request_id, user_id)
		VALUES ($1, $2)
		ON CONFLICT (request_id, user_id) DO NOTHING`

	if _, err := s.pool.Exec(ctx, q, requestID, userID); err != nil {
		return fmt.Errorf("add watcher %d to request %d: %w", userID, requestID, err)
	}
	return nil
}

// RemoveWatcher detaches a user, reporting whether they were the last one.
//
// The second return value is what stops one user's "no thanks" from silently
// destroying two other users' downloads: the caller only cancels the request
// when nobody is left watching it.
func (s *Store) RemoveWatcher(ctx context.Context, requestID, userID int64) (lastOne bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		const del = `DELETE FROM request_watchers WHERE request_id = $1 AND user_id = $2`
		if _, err := tx.Exec(ctx, del, requestID, userID); err != nil {
			return fmt.Errorf("remove watcher: %w", err)
		}

		const count = `SELECT count(*) FROM request_watchers WHERE request_id = $1`
		var remaining int
		if err := tx.QueryRow(ctx, count, requestID).Scan(&remaining); err != nil {
			return fmt.Errorf("count watchers: %w", err)
		}
		lastOne = remaining == 0
		return nil
	})
	return lastOne, err
}

// ListWatchers returns everyone watching a request, with their notification
// preferences.
//
// The join reaches through user_identities for the Telegram id so that the
// notifier does not need a second query per user.
func (s *Store) ListWatchers(ctx context.Context, requestID int64) ([]Watcher, error) {
	const q = `
		SELECT w.user_id,
		       COALESCE(u.username, ''),
		       COALESCE(i.external_id, ''),
		       w.notify_mask
		  FROM request_watchers w
		  JOIN users u ON u.id = w.user_id
		  LEFT JOIN user_identities i
		         ON i.user_id = w.user_id AND i.provider = 'telegram'
		 WHERE w.request_id = $1
		 ORDER BY w.created_at`

	rows, err := s.pool.Query(ctx, q, requestID)
	if err != nil {
		return nil, fmt.Errorf("list watchers for request %d: %w", requestID, err)
	}
	defer rows.Close()

	var out []Watcher
	for rows.Next() {
		var w Watcher
		if err := rows.Scan(&w.UserID, &w.Username, &w.TelegramID, &w.NotifyMask); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

// NotificationKind is why a user is being told something.
type NotificationKind string

const (
	NotificationStarted   NotificationKind = "started"
	NotificationCompleted NotificationKind = "completed"
	NotificationFailed    NotificationKind = "failed"
	NotificationSkipped   NotificationKind = "skipped"
	NotificationCanceled  NotificationKind = "canceled"
)

// Mask returns the notify_mask bit this kind corresponds to, or 0 for a kind
// that is always delivered.
func (k NotificationKind) Mask() int {
	switch k {
	case NotificationStarted:
		return NotifyStarted
	case NotificationCompleted:
		return NotifyCompleted
	case NotificationFailed:
		return NotifyFailed
	default:
		return 0
	}
}

// CreateNotifications writes one notification per watcher.
//
// It returns the rows it created so the caller can hand them to the Telegram
// sender without a second query. A watcher who has switched this kind off is
// skipped, except for the kinds that are always delivered — being told the
// thing you asked for is ready is not something to opt out of.
func (s *Store) CreateNotifications(
	ctx context.Context,
	requestID int64,
	kind NotificationKind,
	title, body string,
	payload any,
) ([]Notification, error) {
	watchers, err := s.ListWatchers(ctx, requestID)
	if err != nil {
		return nil, err
	}

	var created []Notification

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		const q = `
			INSERT INTO notifications (user_id, request_id, kind, title, body, payload)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, created_at`

		for _, w := range watchers {
			if mask := kind.Mask(); mask != 0 && w.NotifyMask&mask == 0 {
				continue
			}

			var (
				id        int64
				createdAt time.Time
			)
			if err := tx.QueryRow(ctx, q,
				w.UserID, requestID, kind, title, nullString(body), payload,
			).Scan(&id, &createdAt); err != nil {
				return fmt.Errorf("create notification for user %d: %w", w.UserID, err)
			}

			created = append(created, Notification{
				ID:         id,
				UserID:     w.UserID,
				RequestID:  requestID,
				Kind:       kind,
				Title:      title,
				Body:       body,
				TelegramID: w.TelegramID,
				CreatedAt:  createdAt,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return created, nil
}

// Notification is a stored notification, with the delivery address attached.
type Notification struct {
	ID         int64
	UserID     int64
	RequestID  int64
	Kind       NotificationKind
	Title      string
	Body       string
	TelegramID string
	CreatedAt  time.Time
}

// MarkTelegramDelivered records that the Telegram copy went out.
//
// A failure here is not worth failing the notification over: the row exists,
// the in-app inbox will show it, and the worst case is a duplicate Telegram
// message on the next attempt.
func (s *Store) MarkTelegramDelivered(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `UPDATE notifications SET delivered_telegram_at = now() WHERE id = ANY($1)`
	if _, err := s.pool.Exec(ctx, q, ids); err != nil {
		return fmt.Errorf("mark notifications delivered: %w", err)
	}
	return nil
}

// ListNotifications returns a user's notifications, newest first.
func (s *Store) ListNotifications(ctx context.Context, userID int64, unreadOnly bool, limit int) ([]Notification, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	q := `
		SELECT id, user_id, COALESCE(request_id, 0), kind, title, COALESCE(body, ''), created_at
		  FROM notifications
		 WHERE user_id = $1`
	if unreadOnly {
		q += ` AND read_at IS NULL`
	}
	q += ` ORDER BY created_at DESC LIMIT $2`

	rows, err := s.pool.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()

	var out []Notification
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.UserID, &n.RequestID, &n.Kind, &n.Title, &n.Body, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Release blacklist
// ---------------------------------------------------------------------------

// BlacklistRelease records a torrent that must not be picked again.
//
// It is global rather than per-request: a release that stalls at zero or dies
// mid-download is dead for everyone, and there is no reason to make the next
// requester rediscover that.
func (s *Store) BlacklistRelease(ctx context.Context, infohash, reason string) error {
	if infohash == "" {
		return nil
	}

	const q = `
		INSERT INTO release_blacklist (infohash, reason)
		VALUES ($1, $2)
		ON CONFLICT (infohash) DO UPDATE SET reason = EXCLUDED.reason`

	if _, err := s.pool.Exec(ctx, q, infohash, reason); err != nil {
		return fmt.Errorf("blacklist release %s: %w", infohash, err)
	}
	return nil
}

// IsBlacklisted reports which of the given identifiers are blacklisted.
//
// It takes a batch because the selector checks a whole search result page at
// once, and a query per release would be tens of round trips per hour per
// request.
func (s *Store) IsBlacklisted(ctx context.Context, identities []string) (map[string]bool, error) {
	out := make(map[string]bool, len(identities))
	if len(identities) == 0 {
		return out, nil
	}

	const q = `SELECT infohash FROM release_blacklist WHERE infohash = ANY($1)`

	rows, err := s.pool.Query(ctx, q, identities)
	if err != nil {
		return nil, fmt.Errorf("check blacklist: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// User is an account.
type User struct {
	ID       int64
	Username string
	IsAdmin  bool
}

// EnsureUserByTelegram finds or creates the user behind a Telegram account.
//
// The identity row is the link between the three interfaces: one user, one
// account, reachable from the bot, the web UI and the CLI alike.
func (s *Store) EnsureUserByTelegram(ctx context.Context, telegramID, username string) (*User, error) {
	return s.ensureUser(ctx, "telegram", telegramID, username)
}

// EnsureUserByCLI finds or creates the user behind a command-line session.
//
// The identity is the operating-system user name, so running the CLI as
// yourself attributes the request to you and the notifications land in your
// inbox — the same account the web UI and the bot reach through their own
// identities. It does mean a request started by a cron job belongs to whoever
// the job runs as, which is the honest answer to "who asked for this".
//
// Created without administrative rights. The first administrator is made
// deliberately, through the web setup, rather than by whoever happens to run
// the CLI first.
func (s *Store) EnsureUserByCLI(ctx context.Context, name string) (*User, error) {
	return s.ensureUser(ctx, "cli", name, name)
}

// FindUserByCLI looks up a CLI user without creating one.
//
// Reads do not create users: a typo in a user name should report that nobody
// is called that, not silently bring an account into existence and then show
// an empty inbox.
func (s *Store) FindUserByCLI(ctx context.Context, name string) (*User, error) {
	const q = `
		SELECT u.id, COALESCE(u.username, ''), u.is_admin
		  FROM users u
		  JOIN user_identities i ON i.user_id = u.id
		 WHERE i.provider = 'cli' AND i.external_id = $1`

	var u User
	err := s.pool.QueryRow(ctx, q, name).Scan(&u.ID, &u.Username, &u.IsAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("cli user %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("find cli user %q: %w", name, err)
	}
	return &u, nil
}

func (s *Store) ensureUser(ctx context.Context, provider, externalID, username string) (*User, error) {
	var out *User

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		const find = `
			SELECT u.id, COALESCE(u.username, ''), u.is_admin
			  FROM users u
			  JOIN user_identities i ON i.user_id = u.id
			 WHERE i.provider = $1 AND i.external_id = $2`

		var u User
		err := tx.QueryRow(ctx, find, provider, externalID).Scan(&u.ID, &u.Username, &u.IsAdmin)
		switch {
		case err == nil:
			out = &u
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("find user by %s id: %w", provider, err)
		}

		const createUser = `
			INSERT INTO users (username, is_admin)
			VALUES ($1, FALSE)
			ON CONFLICT (username) DO NOTHING
			RETURNING id, COALESCE(username, ''), is_admin`

		err = tx.QueryRow(ctx, createUser, nullString(username)).
			Scan(&u.ID, &u.Username, &u.IsAdmin)

		if errors.Is(err, pgx.ErrNoRows) {
			// The user name is unique, and it can legitimately be taken: the
			// same person may already exist from Telegram or the web under the
			// same name. The account is created without one rather than failing
			// the command, and deliberately *without* adopting the existing
			// user — linking two identities is something a person does
			// explicitly, with a code, because doing it by name would let
			// anyone who can run the CLI as a given user take over that
			// account.
			//
			// On conflict rather than catching the error, because Postgres
			// aborts the entire transaction on a failed statement: a second
			// attempt after a unique violation would fail with "current
			// transaction is aborted" rather than inserting anything.
			err = tx.QueryRow(ctx, createUser, nil).
				Scan(&u.ID, &u.Username, &u.IsAdmin)
		}
		if err != nil {
			return fmt.Errorf("create user: %w", err)
		}

		const link = `
			INSERT INTO user_identities (user_id, provider, external_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (provider, external_id) DO UPDATE SET user_id = EXCLUDED.user_id`

		if _, err := tx.Exec(ctx, link, u.ID, provider, externalID); err != nil {
			return fmt.Errorf("link %s identity: %w", provider, err)
		}

		out = &u
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
