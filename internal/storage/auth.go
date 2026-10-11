package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// Accounts for the web interface.
//
// Three tables cooperate and the split matters. `users` holds the account,
// `user_identities` says how somebody proves they are it — a Telegram id, a
// web login, a CLI name — and `sessions` holds one issued cookie. One person
// can therefore reach the same account from all three interfaces without the
// interfaces knowing about each other.

// SessionTTL is how long a login lasts.
//
// Long, because this is a self-hosted tool for one household and being logged
// out weekly is friction with no benefit; the cookie is HttpOnly and the token
// can be revoked by deleting one row.
const SessionTTL = 30 * 24 * time.Hour

// ErrUnauthorized is returned when credentials do not match.
//
// One error for a wrong username and for a wrong password, deliberately: a
// caller that can distinguish them tells an attacker which usernames exist.
var ErrUnauthorized = errors.New("invalid credentials")

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

// HasAccounts reports whether the instance has been set up.
//
// It asks whether any account can be logged into from the web, not whether any
// user row exists: a Telegram user is created automatically the first time
// somebody talks to the bot, and that must not count as having completed setup
// and lock the operator out of the wizard.
func (s *Store) HasAccounts(ctx context.Context) (bool, error) {
	const q = `SELECT EXISTS (SELECT 1 FROM users WHERE password_hash IS NOT NULL)`

	var exists bool
	if err := s.pool.QueryRow(ctx, q).Scan(&exists); err != nil {
		return false, fmt.Errorf("check for accounts: %w", err)
	}
	return exists, nil
}

// CreateAccount creates a login and the identity that reaches it.
//
// isAdmin is passed rather than decided here: the first account is created by
// the setup wizard and is an administrator by definition, and every later one
// comes from an invite and is not.
func (s *Store) CreateAccount(ctx context.Context, username, password string, isAdmin bool) (*User, error) {
	username = strings.TrimSpace(username)
	if err := validateUsername(username); err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash the password: %w", err)
	}

	var out *User
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		const create = `
			INSERT INTO users (username, password_hash, is_admin)
			VALUES ($1, $2, $3)
			RETURNING id, COALESCE(username, ''), is_admin`

		var u User
		err := tx.QueryRow(ctx, create, username, string(hash), isAdmin).
			Scan(&u.ID, &u.Username, &u.IsAdmin)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("the name %q is taken", username)
			}
			return fmt.Errorf("create account: %w", err)
		}

		const link = `
			INSERT INTO user_identities (user_id, provider, external_id)
			VALUES ($1, 'web', $2)`

		if _, err := tx.Exec(ctx, link, u.ID, username); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("the name %q is taken", username)
			}
			return fmt.Errorf("link the web identity: %w", err)
		}

		out = &u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Authenticate checks a username and password.
func (s *Store) Authenticate(ctx context.Context, username, password string) (*User, error) {
	const q = `
		SELECT id, COALESCE(username, ''), is_admin, COALESCE(password_hash, '')
		  FROM users
		 WHERE username = $1`

	var (
		u    User
		hash string
	)
	err := s.pool.QueryRow(ctx, q, strings.TrimSpace(username)).
		Scan(&u.ID, &u.Username, &u.IsAdmin, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Hash a dummy value before returning, so that a missing account and a
		// wrong password take the same time. Without it, response latency
		// answers "does this username exist", which is the question the single
		// shared error above exists to refuse.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("look up account: %w", err)
	}

	if hash == "" {
		// An account that exists only as a Telegram or CLI identity. It cannot
		// be logged into until a password is set, and saying so is worse than
		// saying nothing.
		return nil, ErrUnauthorized
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, ErrUnauthorized
	}
	return &u, nil
}

// dummyHash is a valid bcrypt hash of a value nothing will match, used to keep
// the timing of a missing account level with a wrong password.
var dummyHash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")

// SetPassword sets or replaces an account's password.
func (s *Store) SetPassword(ctx context.Context, userID int64, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash the password: %w", err)
	}

	const q = `UPDATE users SET password_hash = $2 WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, userID, string(hash))
	if err != nil {
		return fmt.Errorf("set the password for user %d: %w", userID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user %d: %w", userID, ErrNotFound)
	}
	return nil
}

// The rules, as numbers rather than as sentences.
//
// The messages live in each interface instead. They have to: the CLI is in
// English and the web interface is in Russian, so a sentence written here
// would be shown in the wrong language by one of them. What is shared is the
// policy, which is the part that must not drift.
const (
	// MinPasswordLength is the only password rule. Length and nothing else:
	// composition rules — a digit, a symbol, mixed case — are documented to
	// push people towards shorter, more predictable passwords. What is
	// enforced is a floor high enough to make guessing hopeless.
	MinPasswordLength = 10

	MinUsernameLength = 3
	MaxUsernameLength = 64
)

// ValidatePassword applies the password rule.
func ValidatePassword(password string) error {
	if len([]rune(password)) < MinPasswordLength {
		return fmt.Errorf("a password needs at least %d characters", MinPasswordLength)
	}
	return nil
}

// validateUsername rejects names that cannot be logged in with.
//
// The rules are about the login form rather than about taste: a name with
// leading or trailing space cannot be typed back reliably, and an empty one
// renders as a blank row in a list of accounts.
func validateUsername(name string) error {
	switch {
	case name == "":
		return errors.New("a name is required")
	case len([]rune(name)) < MinUsernameLength:
		return fmt.Errorf("a name needs at least %d characters", MinUsernameLength)
	case len(name) > MaxUsernameLength:
		return fmt.Errorf("a name may be at most %d characters", MaxUsernameLength)
	case strings.ContainsAny(name, " \t\n"):
		return errors.New("a name may not contain spaces")
	}
	return nil
}

// ValidUsername reports whether a name can be used, for an interface that
// wants to check before submitting.
func ValidUsername(name string) error { return validateUsername(strings.TrimSpace(name)) }

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// CreateSession issues a session token and returns the value to put in the
// cookie.
//
// The database stores a SHA-256 of the token rather than the token itself, so
// that a leaked database — a backup, a `pg_dump` in a ticket — does not hand
// over live sessions. The cookie is the only place the original exists.
func (s *Store) CreateSession(ctx context.Context, userID int64) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate a session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	const q = `INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, $3)`
	if _, err := s.pool.Exec(ctx, q, hashToken(token), userID,
		time.Now().Add(SessionTTL)); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// SessionUser resolves a cookie token to its account.
//
// An expired session is deleted on the way past rather than merely ignored:
// nothing else cleans them up, and without this they accumulate for the life
// of the instance.
func (s *Store) SessionUser(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrUnauthorized
	}

	const q = `
		SELECT u.id, COALESCE(u.username, ''), u.is_admin, s.expires_at
		  FROM sessions s
		  JOIN users u ON u.id = s.user_id
		 WHERE s.token = $1`

	var (
		u       User
		expires time.Time
	)
	err := s.pool.QueryRow(ctx, q, hashToken(token)).Scan(&u.ID, &u.Username, &u.IsAdmin, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("resolve session: %w", err)
	}

	if time.Now().After(expires) {
		if err := s.DeleteSession(ctx, token); err != nil {
			s.log.Warn("could not delete an expired session", "error", err)
		}
		return nil, ErrUnauthorized
	}
	return &u, nil
}

// DeleteSession revokes one session.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	const q = `DELETE FROM sessions WHERE token = $1`
	if _, err := s.pool.Exec(ctx, q, hashToken(token)); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteUserSessions revokes every session for an account.
//
// Used when a password changes: sessions are bearer tokens, so changing the
// password without this would leave anyone already holding one logged in.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	const q = `DELETE FROM sessions WHERE user_id = $1`
	if _, err := s.pool.Exec(ctx, q, userID); err != nil {
		return fmt.Errorf("delete sessions for user %d: %w", userID, err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions that have lapsed.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	const q = `DELETE FROM sessions WHERE expires_at < now()`
	tag, err := s.pool.Exec(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// hashToken is what goes in the token column.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Invites
// ---------------------------------------------------------------------------

// InviteTTL is how long an unused invite stays valid.
const InviteTTL = 7 * 24 * time.Hour

// CreateInvite issues a code that lets somebody create an account.
//
// The instance is not open for registration: it is somebody's home server, and
// an open signup form on it is an open signup form on their Plex library. The
// code is the only way in, and it is generated by an administrator.
func (s *Store) CreateInvite(ctx context.Context, createdBy int64) (string, error) {
	raw := make([]byte, 9)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate an invite code: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)

	const q = `INSERT INTO invite_codes (code, created_by, expires_at) VALUES ($1, $2, $3)`
	if _, err := s.pool.Exec(ctx, q, code, createdBy, time.Now().Add(InviteTTL)); err != nil {
		return "", fmt.Errorf("create invite: %w", err)
	}
	return code, nil
}

// Invite is a code and what is known about it.
type Invite struct {
	Code      string
	CreatedBy int64
	ExpiresAt time.Time
	UsedBy    *int64
	UsedAt    *time.Time
	CreatedAt time.Time
}

// GetInvite looks a code up without using it.
func (s *Store) GetInvite(ctx context.Context, code string) (*Invite, error) {
	const q = `
		SELECT code, created_by, expires_at, used_by, used_at, created_at
		  FROM invite_codes WHERE code = $1`

	var inv Invite
	err := s.pool.QueryRow(ctx, q, code).Scan(&inv.Code, &inv.CreatedBy, &inv.ExpiresAt,
		&inv.UsedBy, &inv.UsedAt, &inv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("invite: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get invite: %w", err)
	}
	return &inv, nil
}

// Usable reports whether an invite can still be redeemed, and why not if it
// cannot.
func (i *Invite) Usable(now time.Time) error {
	switch {
	case i.UsedBy != nil:
		return errors.New("this invite has already been used")
	case now.After(i.ExpiresAt):
		return errors.New("this invite has expired")
	default:
		return nil
	}
}

// ListInvites returns the invites, newest first.
func (s *Store) ListInvites(ctx context.Context, limit int) ([]Invite, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	q := `
		SELECT code, created_by, expires_at, used_by, used_at, created_at
		  FROM invite_codes
		 ORDER BY created_at DESC
		 LIMIT $1`

	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()

	var out []Invite
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.Code, &inv.CreatedBy, &inv.ExpiresAt,
			&inv.UsedBy, &inv.UsedAt, &inv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RedeemInvite consumes a code and creates the account it unlocks, in one
// transaction.
//
// Together, because the alternative is a code marked used with no account
// behind it — the person is locked out and the administrator has to issue
// another — or an account created from a code somebody else consumed first.
// The row is locked before it is checked for exactly that race.
func (s *Store) RedeemInvite(ctx context.Context, code, username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if err := validateUsername(username); err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash the password: %w", err)
	}

	var out *User

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		const lock = `
			SELECT used_by, expires_at FROM invite_codes WHERE code = $1 FOR UPDATE`

		var (
			usedBy  *int64
			expires time.Time
		)
		err := tx.QueryRow(ctx, lock, code).Scan(&usedBy, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("that invite is not valid")
		}
		if err != nil {
			return fmt.Errorf("lock invite: %w", err)
		}

		if err := (&Invite{UsedBy: usedBy, ExpiresAt: expires}).Usable(time.Now()); err != nil {
			return err
		}

		const create = `
			INSERT INTO users (username, password_hash, is_admin)
			VALUES ($1, $2, FALSE)
			RETURNING id, COALESCE(username, ''), is_admin`

		var u User
		if err := tx.QueryRow(ctx, create, username, string(hash)).
			Scan(&u.ID, &u.Username, &u.IsAdmin); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("the name %q is taken", username)
			}
			return fmt.Errorf("create account: %w", err)
		}

		const link = `
			INSERT INTO user_identities (user_id, provider, external_id)
			VALUES ($1, 'web', $2)`

		if _, err := tx.Exec(ctx, link, u.ID, username); err != nil {
			return fmt.Errorf("link the web identity: %w", err)
		}

		const use = `UPDATE invite_codes SET used_by = $2, used_at = now() WHERE code = $1`
		if _, err := tx.Exec(ctx, use, code, u.ID); err != nil {
			return fmt.Errorf("mark the invite used: %w", err)
		}

		out = &u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListUsers returns the accounts, oldest first.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	const q = `
		SELECT id, COALESCE(username, ''), is_admin
		  FROM users
		 ORDER BY created_at`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.IsAdmin); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUser loads one account.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	const q = `SELECT id, COALESCE(username, ''), is_admin FROM users WHERE id = $1`

	var u User
	err := s.pool.QueryRow(ctx, q, id).Scan(&u.ID, &u.Username, &u.IsAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("user %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get user %d: %w", id, err)
	}
	return &u, nil
}

// ---------------------------------------------------------------------------
// Notifications, for the web inbox
// ---------------------------------------------------------------------------

// MarkNotificationsRead marks specific notifications read, for one user.
//
// The user id is part of the statement rather than checked beforehand: a
// notification id is visible in a URL, and without this anyone who guessed one
// could clear somebody else's inbox.
func (s *Store) MarkNotificationsRead(ctx context.Context, userID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	const q = `
		UPDATE notifications SET read_at = now()
		 WHERE user_id = $1 AND id = ANY($2) AND read_at IS NULL`

	if _, err := s.pool.Exec(ctx, q, userID, ids); err != nil {
		return fmt.Errorf("mark notifications read: %w", err)
	}
	return nil
}

// MarkAllNotificationsRead clears a user's inbox.
func (s *Store) MarkAllNotificationsRead(ctx context.Context, userID int64) error {
	const q = `UPDATE notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL`
	if _, err := s.pool.Exec(ctx, q, userID); err != nil {
		return fmt.Errorf("mark all notifications read: %w", err)
	}
	return nil
}

// UnreadCount returns how many notifications a user has not read.
func (s *Store) UnreadCount(ctx context.Context, userID int64) (int, error) {
	const q = `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`

	var n int
	if err := s.pool.QueryRow(ctx, q, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return n, nil
}
