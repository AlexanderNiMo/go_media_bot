package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestLoginSerializesWithPasswordUpdates(t *testing.T) {
	for _, update := range []string{"change", "reissue"} {
		t.Run(update, func(t *testing.T) {
			store, pool := newStoreFor(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			admin, err := store.CreateAccount(ctx, "admin", "admin-password-long", true)
			if err != nil {
				t.Fatal(err)
			}
			code, err := store.CreateInvite(ctx, admin.ID)
			if err != nil {
				t.Fatal(err)
			}
			user, temporary, err := store.RedeemTelegramInvite(ctx, code, "777", "alice")
			if err != nil {
				t.Fatal(err)
			}

			// Pause login at session insertion, after its password check.
			if _, err := pool.Exec(ctx, `
				CREATE FUNCTION pause_session() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN PERFORM pg_advisory_xact_lock(73191); RETURN NEW; END $$;
				CREATE TRIGGER pause_session BEFORE INSERT ON sessions
				FOR EACH ROW EXECUTE FUNCTION pause_session()`); err != nil {
				t.Fatal(err)
			}
			blocker, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, "SELECT pg_advisory_xact_lock(73191)"); err != nil {
				t.Fatal(err)
			}
			type result struct {
				token string
				err   error
			}
			done := make(chan result, 1)
			go func() {
				_, token, err := store.Login(ctx, "alice", temporary)
				done <- result{token, err}
			}()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
					WHERE locktype = 'advisory' AND objid = 73191 AND NOT granted
					AND database = (SELECT oid FROM pg_database WHERE datname = current_database()))`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case r := <-done:
					t.Fatalf("login finished before session insertion: %v", r.err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			// A password update needs this same lock. It must not overtake login.
			_, err = pool.Exec(ctx, "SELECT id FROM users WHERE id = $1 FOR UPDATE NOWAIT", user.ID)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				t.Fatalf("login released the user lock before storing its session: %v", err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			r := <-done
			if r.err != nil {
				t.Fatal(r.err)
			}
			if _, err := store.SessionUser(ctx, r.token); err != nil {
				t.Fatal(err)
			}
			if update == "change" {
				err = store.ChangePassword(ctx, user.ID, temporary, "permanent-password")
			} else {
				_, _, err = store.RedeemTelegramInvite(ctx, code, "777", "alice")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SessionUser(ctx, r.token); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("session survived password update: %v", err)
			}
			if _, token, err := store.Login(ctx, "alice", temporary); !errors.Is(err, ErrUnauthorized) || token != "" {
				t.Fatalf("old password issued a session: %v", err)
			}
		})
	}
}

func TestChangeTemporaryPasswordRequiresLiveTemporarySession(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()
	admin, err := store.CreateAccount(ctx, "admin", "admin-password-long", true)
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.CreateInvite(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	user, temporary, err := store.RedeemTelegramInvite(ctx, code, "777", "alice")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := store.Login(ctx, "alice", temporary)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateSession(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := store.CreateSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 second' WHERE token = $1", hashToken(expired)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "missing", other, expired} {
		if err := store.ChangeTemporaryPassword(ctx, user.ID, invalid, "permanent-password"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("invalid session accepted: %v", err)
		}
	}
	if err := store.ChangeTemporaryPassword(ctx, user.ID, token, temporary); !errors.Is(err, ErrPasswordUnchanged) {
		t.Fatalf("temporary password reused: %v", err)
	}
	_, replacement, err := store.RedeemTelegramInvite(ctx, code, "777", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ChangeTemporaryPassword(ctx, user.ID, token, "permanent-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reissued password's revoked session accepted: %v", err)
	}
	_, token, err = store.Login(ctx, "alice", replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ChangeTemporaryPassword(ctx, user.ID, token, "permanent-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionUser(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("password change did not revoke session")
	}
	_, token, err = store.Login(ctx, "alice", "permanent-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ChangeTemporaryPassword(ctx, user.ID, token, "another-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("permanent account changed password without current one: %v", err)
	}
	if err := store.ChangePassword(ctx, user.ID, "", "another-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ordinary password change accepted empty current password: %v", err)
	}
}
