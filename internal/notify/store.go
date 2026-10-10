package notify

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"babki.my/babki/internal/platform/db"
)

// Subscription is one device a member turned reminders on for.
type Subscription struct {
	Endpoint string
	UserID   uuid.UUID
	P256dh   string
	Auth     string
}

// Store keeps the devices and the reminders sent.
type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

// Subscribe keeps a device for a member, or moves it to them: one endpoint is
// one device.
func (s *Store) Subscribe(ctx context.Context, spaceID uuid.UUID, sub Subscription) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO push_subscriptions (endpoint, user_id, space_id, p256dh, auth) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (endpoint) DO UPDATE SET user_id = EXCLUDED.user_id, space_id = EXCLUDED.space_id,
			p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth`,
		sub.Endpoint, sub.UserID, spaceID, sub.P256dh, sub.Auth)
	if err != nil {
		return fmt.Errorf("notify: subscribe: %w", err)
	}
	return nil
}

// Unsubscribe forgets a member's device; false when it was not theirs.
func (s *Store) Unsubscribe(ctx context.Context, userID uuid.UUID, endpoint string) (bool, error) {
	ct, err := s.db.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint = $1 AND user_id = $2`, endpoint, userID)
	if err != nil {
		return false, fmt.Errorf("notify: unsubscribe: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

// Forget drops a device its push service says is gone.
func (s *Store) Forget(ctx context.Context, endpoint string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint); err != nil {
		return fmt.Errorf("notify: forget: %w", err)
	}
	return nil
}

// Spaces are the spaces with a device to send to.
func (s *Store) Spaces(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT space_id FROM push_subscriptions`)
	if err != nil {
		return nil, fmt.Errorf("notify: spaces: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("notify: spaces: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Devices are a space's devices, or one member's when userID is set.
func (s *Store) Devices(ctx context.Context, spaceID uuid.UUID, userID *uuid.UUID) ([]Subscription, error) {
	rows, err := s.db.Query(ctx, `
		SELECT endpoint, user_id, p256dh, auth FROM push_subscriptions
		WHERE space_id = $1 AND ($2::uuid IS NULL OR user_id = $2) ORDER BY created_at`, spaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("notify: devices: %w", err)
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.Endpoint, &sub.UserID, &sub.P256dh, &sub.Auth); err != nil {
			return nil, fmt.Errorf("notify: devices: %w", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// Sent says whether the reminder went to the member already.
func (s *Store) Sent(ctx context.Context, userID uuid.UUID, key string) (bool, error) {
	var sent bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM push_sent WHERE user_id = $1 AND key = $2)`, userID, key).Scan(&sent)
	if err != nil {
		return false, fmt.Errorf("notify: sent: %w", err)
	}
	return sent, nil
}

// MarkSent notes the reminder went to the member.
func (s *Store) MarkSent(ctx context.Context, userID uuid.UUID, key string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO push_sent (user_id, key) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, key)
	if err != nil {
		return fmt.Errorf("notify: mark sent: %w", err)
	}
	return nil
}
