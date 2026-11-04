package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTelegramRegistration(t *testing.T) {
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

	if _, err := store.LinkTelegramByCode(ctx, code, "777", "alice"); err == nil {
		t.Fatal("registration invite accepted as a link code")
	}
	for _, name := range []string{"", "admin"} {
		if _, _, err := store.RedeemTelegramInvite(ctx, code, "777", name); err == nil {
			t.Fatal("invalid login accepted")
		}
		inv, err := store.GetInvite(ctx, code)
		if err != nil || inv.UsedBy != nil {
			t.Fatalf("failed registration spent invite: %v", err)
		}
	}
	u, password, err := store.RedeemTelegramInvite(ctx, code, "777", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.IsAdmin || !u.MustChangePassword {
		t.Fatalf("wrong account flags: %+v", u)
	}
	if _, err := store.Authenticate(ctx, "alice", password); err != nil {
		t.Fatal(err)
	}
	tg, err := store.UserByTelegram(ctx, "777")
	if err != nil || tg.ID != u.ID {
		t.Fatalf("telegram identity: %v", err)
	}
	var identities int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM user_identities WHERE user_id = $1", u.ID).Scan(&identities); err != nil || identities != 2 {
		t.Fatalf("identities = %d, err = %v", identities, err)
	}
	token, err := store.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RedeemTelegramInvite(ctx, code, "888", "intruder"); err == nil {
		t.Fatal("another Telegram reissued password")
	}
	_, replacement, err := store.RedeemTelegramInvite(ctx, code, "777", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, "alice", password); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old password works")
	}
	if _, err := store.SessionUser(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old session works")
	}
	if err := store.ChangePassword(ctx, u.ID, replacement, replacement); err == nil {
		t.Fatal("unchanged password accepted")
	}
	if err := store.ChangePassword(ctx, u.ID, "wrong", "new-password-long"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong password: %v", err)
	}
	if err := store.ChangePassword(ctx, u.ID, replacement, "new-password-long"); err != nil {
		t.Fatal(err)
	}
	logged, err := store.Authenticate(ctx, "alice", "new-password-long")
	if err != nil || logged.MustChangePassword {
		t.Fatalf("password change: %v", err)
	}
	if _, _, err := store.RedeemTelegramInvite(ctx, code, "777", "alice"); err == nil {
		t.Fatal("reissued permanent password")
	}

	link, err := store.CreateLinkCode(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RedeemTelegramInvite(ctx, link, "999", "bob"); err == nil {
		t.Fatal("link code accepted as registration")
	}
	if _, err := store.RedeemInvite(ctx, link, "bob", "bob-password-long"); err == nil {
		t.Fatal("link code accepted by web registration")
	}
	if _, err := store.GetInvite(ctx, link); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link code exposed by registration lookup: %v", err)
	}
	linked, err := store.LinkTelegramByCode(ctx, link, "999", "admin")
	if err != nil || linked.ID != admin.ID {
		t.Fatalf("link existing web account: %v", err)
	}

	for _, c := range []string{"missing", "expired"} {
		if c == "expired" {
			c, err = store.CreateInvite(ctx, admin.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, "UPDATE invite_codes SET expires_at = now() - interval '1 day' WHERE code = $1", c); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := store.RedeemTelegramInvite(ctx, c, "1000", "carol"); err == nil {
			t.Fatal("invalid invite accepted")
		}
	}
}

func TestTelegramInviteConcurrentRedemption(t *testing.T) {
	store, _ := newStoreFor(t)
	ctx := context.Background()
	admin, err := store.CreateAccount(ctx, "admin", "admin-password-long", true)
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.CreateInvite(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := store.RedeemTelegramInvite(ctx, code, fmt.Sprint(i+1), fmt.Sprintf("user%d", i))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var winners int
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d winners, want one", winners)
	}
}

func TestTelegramRegistrationMigrationExpiresOnlyLegacyCodes(t *testing.T) {
	store, pool := newStoreFor(t)
	ctx := context.Background()
	admin, err := store.CreateAccount(ctx, "admin", "admin-password-long", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "ALTER TABLE invite_codes DROP COLUMN purpose"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO invite_codes (code, created_by, expires_at) VALUES ('legacy', $1, now() + interval '7 days')", admin.ID); err != nil {
		t.Fatal(err)
	}
	data, err := migrationsFS.ReadFile("migrations/00013_telegram_registration.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(data), "-- +goose Down")
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.GetInvite(ctx, "legacy")
	if err != nil || legacy.Usable(time.Now()) == nil {
		t.Fatalf("legacy code still usable: %v", err)
	}
	code, err := store.CreateInvite(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.GetInvite(ctx, code)
	if err != nil || fresh.Usable(time.Now()) != nil {
		t.Fatalf("rerun expired a new code: %v", err)
	}
}
