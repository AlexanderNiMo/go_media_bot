package storage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// RedeemTelegramInvite creates both identities, or reissues an undelivered
// temporary password to the same Telegram identity before its first change.
func (s *Store) RedeemTelegramInvite(ctx context.Context, code, telegramID, username string) (*User, string, error) {
	code = strings.TrimSpace(code)
	username = strings.TrimSpace(username)
	if code == "" || telegramID == "" {
		return nil, "", errors.New("нужны код приглашения и Telegram ID")
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("generate temporary password: %w", err)
	}
	password := base64.RawURLEncoding.EncodeToString(raw)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", fmt.Errorf("hash temporary password: %w", err)
	}
	var u User
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var usedBy *int64
		var expires time.Time
		err := tx.QueryRow(ctx, `SELECT used_by, expires_at FROM invite_codes
			WHERE code = $1 AND purpose = 'registration' FOR UPDATE`, code).Scan(&usedBy, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("приглашение недействительно")
		}
		if err != nil {
			return fmt.Errorf("lock registration invite: %w", err)
		}
		if usedBy != nil {
			err := tx.QueryRow(ctx, `SELECT u.id, COALESCE(u.username, ''), u.is_admin, u.must_change_password
				FROM users u JOIN user_identities i ON i.user_id = u.id
				WHERE u.id = $1 AND i.provider = 'telegram' AND i.external_id = $2
				FOR UPDATE OF u`, *usedBy, telegramID).
				Scan(&u.ID, &u.Username, &u.IsAdmin, &u.MustChangePassword)
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("приглашение уже использовано")
			}
			if err != nil {
				return fmt.Errorf("find registration recipient: %w", err)
			}
			if !u.MustChangePassword {
				return errors.New("регистрация уже завершена; используйте свой пароль для входа")
			}
			if _, err := tx.Exec(ctx, "UPDATE users SET password_hash = $2 WHERE id = $1", u.ID, string(hash)); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1", u.ID)
			return err
		}
		if err := (&Invite{ExpiresAt: expires}).Usable(time.Now()); err != nil {
			return errors.New("срок приглашения истёк")
		}
		if err := validateUsername(username); err != nil {
			return errors.New("укажите логин: /join <код> <логин> (от 3 до 64 символов, без пробелов)")
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_identities
			WHERE provider = 'telegram' AND external_id = $1)`, telegramID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errors.New("этот Telegram уже привязан к аккаунту")
		}
		err = tx.QueryRow(ctx, `INSERT INTO users (username, password_hash, must_change_password)
			VALUES ($1, $2, TRUE) RETURNING id, username, is_admin, must_change_password`,
			username, string(hash)).Scan(&u.ID, &u.Username, &u.IsAdmin, &u.MustChangePassword)
		if isUniqueViolation(err) {
			return errors.New("это имя занято; отправьте /join <код> <другой_логин>")
		}
		if err != nil {
			return fmt.Errorf("create telegram account: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_identities (user_id, provider, external_id)
			VALUES ($1, 'web', $2), ($1, 'telegram', $3)`, u.ID, username, telegramID); err != nil {
			if isUniqueViolation(err) {
				return errors.New("логин или Telegram уже привязан к аккаунту")
			}
			return fmt.Errorf("create account identities: %w", err)
		}
		_, err = tx.Exec(ctx, "UPDATE invite_codes SET used_by = $2, used_at = now() WHERE code = $1", code, u.ID)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return &u, password, nil
}
