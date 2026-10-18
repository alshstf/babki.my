package notify

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Bot is a space's Telegram bot: its sealed token, the token's tail the
// owner is shown, its name and the next update to read.
type Bot struct {
	SpaceID         uuid.UUID
	TokenCiphertext []byte
	TokenLast4      string
	Username        string
	Offset          int64
}

// Chat is a member's chat with the space's bot.
type Chat struct {
	UserID, SpaceID uuid.UUID
	ChatID          int64
}

// linkTTL is how long a code to tie a chat lasts.
const linkTTL = 15 * time.Minute

// Bot is the space's bot, nil without one.
func (s *Store) Bot(ctx context.Context, spaceID uuid.UUID) (*Bot, error) {
	b := Bot{SpaceID: spaceID}
	err := s.db.QueryRow(ctx, `SELECT token_ciphertext, token_last4, username, update_offset FROM telegram_bots WHERE space_id = $1`, spaceID).
		Scan(&b.TokenCiphertext, &b.TokenLast4, &b.Username, &b.Offset)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("notify: bot: %w", err)
	}
	return &b, nil
}

// Bots are every space's bot, for reading what was sent to them.
func (s *Store) Bots(ctx context.Context) ([]Bot, error) {
	rows, err := s.db.Query(ctx, `SELECT space_id, token_ciphertext, token_last4, username, update_offset FROM telegram_bots ORDER BY space_id`)
	if err != nil {
		return nil, fmt.Errorf("notify: bots: %w", err)
	}
	defer rows.Close()
	var out []Bot
	for rows.Next() {
		var b Bot
		if err := rows.Scan(&b.SpaceID, &b.TokenCiphertext, &b.TokenLast4, &b.Username, &b.Offset); err != nil {
			return nil, fmt.Errorf("notify: bots: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetBot keeps the space's bot in place of any before it; the chats tied to
// the old one go with it — a new bot has chats of its own.
func (s *Store) SetBot(ctx context.Context, b Bot) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var old string
	err = tx.QueryRow(ctx, `SELECT username FROM telegram_bots WHERE space_id = $1`, b.SpaceID).Scan(&old)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("notify: set bot: %w", err)
	}
	if old != b.Username {
		if _, err := tx.Exec(ctx, `DELETE FROM telegram_chats WHERE space_id = $1`, b.SpaceID); err != nil {
			return fmt.Errorf("notify: set bot: %w", err)
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO telegram_bots (space_id, token_ciphertext, token_last4, username) VALUES ($1, $2, $3, $4)
		ON CONFLICT (space_id) DO UPDATE SET token_ciphertext = EXCLUDED.token_ciphertext, token_last4 = EXCLUDED.token_last4,
			username = EXCLUDED.username, update_offset = CASE WHEN telegram_bots.username = EXCLUDED.username THEN telegram_bots.update_offset ELSE 0 END`,
		b.SpaceID, b.TokenCiphertext, b.TokenLast4, b.Username)
	if err != nil {
		return fmt.Errorf("notify: set bot: %w", err)
	}
	return tx.Commit(ctx)
}

// DeleteBot forgets the space's bot and the chats tied to it; false without
// one.
func (s *Store) DeleteBot(ctx context.Context, spaceID uuid.UUID) (bool, error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM telegram_chats WHERE space_id = $1`, spaceID); err != nil {
		return false, fmt.Errorf("notify: delete bot: %w", err)
	}
	ct, err := s.db.Exec(ctx, `DELETE FROM telegram_bots WHERE space_id = $1`, spaceID)
	if err != nil {
		return false, fmt.Errorf("notify: delete bot: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

// SetOffset notes the next update of the space's bot to read.
func (s *Store) SetOffset(ctx context.Context, spaceID uuid.UUID, offset int64) error {
	if _, err := s.db.Exec(ctx, `UPDATE telegram_bots SET update_offset = $2 WHERE space_id = $1`, spaceID, offset); err != nil {
		return fmt.Errorf("notify: set offset: %w", err)
	}
	return nil
}

// NewLinkCode gives the member a code to tie a chat with, in place of one
// given before.
func (s *Store) NewLinkCode(ctx context.Context, spaceID, userID uuid.UUID, now time.Time) (string, time.Time, error) {
	raw := make([]byte, 10)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	code := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	expires := now.Add(linkTTL)
	if _, err := s.db.Exec(ctx, `DELETE FROM telegram_link_codes WHERE user_id = $1 OR expires_at < $2`, userID, now); err != nil {
		return "", time.Time{}, fmt.Errorf("notify: link code: %w", err)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO telegram_link_codes (code, user_id, space_id, expires_at) VALUES ($1, $2, $3, $4)`,
		code, userID, spaceID, expires); err != nil {
		return "", time.Time{}, fmt.Errorf("notify: link code: %w", err)
	}
	return code, expires, nil
}

// Link ties chatID to the member whose code it is, within the space and the
// code's quarter of an hour; the code is spent. ok is false for a code that
// is not, or no more.
func (s *Store) Link(ctx context.Context, spaceID uuid.UUID, code string, chatID int64, now time.Time) (bool, error) {
	var userID uuid.UUID
	err := s.db.QueryRow(ctx, `DELETE FROM telegram_link_codes WHERE code = $1 AND space_id = $2 AND expires_at >= $3 RETURNING user_id`,
		strings.ToLower(strings.TrimSpace(code)), spaceID, now).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("notify: link: %w", err)
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO telegram_chats (user_id, space_id, chat_id) VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET space_id = EXCLUDED.space_id, chat_id = EXCLUDED.chat_id, linked_at = now()`,
		userID, spaceID, chatID)
	if err != nil {
		return false, fmt.Errorf("notify: link: %w", err)
	}
	return true, nil
}

// ChatOf is the member's chat, nil without one.
func (s *Store) ChatOf(ctx context.Context, userID uuid.UUID) (*Chat, error) {
	c := Chat{UserID: userID}
	err := s.db.QueryRow(ctx, `SELECT space_id, chat_id FROM telegram_chats WHERE user_id = $1`, userID).Scan(&c.SpaceID, &c.ChatID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("notify: chat: %w", err)
	}
	return &c, nil
}

// Chats are the space's members' chats.
func (s *Store) Chats(ctx context.Context, spaceID uuid.UUID) ([]Chat, error) {
	rows, err := s.db.Query(ctx, `SELECT user_id, chat_id FROM telegram_chats WHERE space_id = $1 ORDER BY linked_at`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("notify: chats: %w", err)
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		c := Chat{SpaceID: spaceID}
		if err := rows.Scan(&c.UserID, &c.ChatID); err != nil {
			return nil, fmt.Errorf("notify: chats: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Unlink forgets the member's chat; false without one.
func (s *Store) Unlink(ctx context.Context, userID uuid.UUID) (bool, error) {
	ct, err := s.db.Exec(ctx, `DELETE FROM telegram_chats WHERE user_id = $1`, userID)
	if err != nil {
		return false, fmt.Errorf("notify: unlink: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

// sealer opens a token with any known key and seals it with the current one.
type sealer interface {
	Open(sealed []byte) ([]byte, error)
	Seal(plaintext []byte) []byte
}

// ResealBots re-encrypts every bot's token with the current key, so an old
// key can be dropped; all or none. Returns how many.
func (s *Store) ResealBots(ctx context.Context, box sealer) (int, error) {
	bots, err := s.Bots(ctx)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, b := range bots {
		plain, err := box.Open(b.TokenCiphertext)
		if err != nil {
			return 0, fmt.Errorf("notify: bot of space %s: no key opens its token: %w", b.SpaceID, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE telegram_bots SET token_ciphertext = $2 WHERE space_id = $1`, b.SpaceID, box.Seal(plain)); err != nil {
			return 0, fmt.Errorf("notify: reseal bot of space %s: %w", b.SpaceID, err)
		}
	}
	return len(bots), tx.Commit(ctx)
}
