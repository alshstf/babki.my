package recurring

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
)

// Key names a regular payment for good: its payee as the list folds it, which
// way the money goes and in what currency.
type Key struct {
	Payee    string
	Incoming bool
	Currency string
}

// KeyOf is the key of a payment shown under name.
func KeyOf(name string, incoming bool, currency string) Key {
	return Key{Payee: fold(name), Incoming: incoming, Currency: currency}
}

func fold(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "ё", "е")
}

// Hidden keeps the payments the family said are not regular.
type Hidden struct{ db db.Executor }

func NewHidden(x db.Executor) *Hidden { return &Hidden{db: x} }

// All is the space's hidden payments.
func (h *Hidden) All(ctx context.Context, spaceID uuid.UUID) (map[Key]bool, error) {
	rows, err := h.db.Query(ctx, `SELECT payee, incoming, currency FROM recurring_hidden WHERE space_id = $1`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("recurring: hidden: %w", err)
	}
	defer rows.Close()
	out := map[Key]bool{}
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.Payee, &k.Incoming, &k.Currency); err != nil {
			return nil, fmt.Errorf("recurring: hidden: %w", err)
		}
		out[k] = true
	}
	return out, rows.Err()
}

// Hide says the payment is not regular.
func (h *Hidden) Hide(ctx context.Context, spaceID uuid.UUID, k Key) error {
	if k.Payee == "" || len([]rune(k.Payee)) > 200 || k.Currency == "" {
		return fmt.Errorf("%w: a payment is named by its payee and currency", family.ErrValidation)
	}
	_, err := h.db.Exec(ctx, `INSERT INTO recurring_hidden (space_id, payee, incoming, currency) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, spaceID, k.Payee, k.Incoming, k.Currency)
	if err != nil {
		return fmt.Errorf("recurring: hide: %w", err)
	}
	return nil
}

// Show takes a payment back among the regular ones; false when it was not
// hidden.
func (h *Hidden) Show(ctx context.Context, spaceID uuid.UUID, k Key) (bool, error) {
	ct, err := h.db.Exec(ctx, `DELETE FROM recurring_hidden WHERE space_id = $1 AND payee = $2 AND incoming = $3 AND currency = $4`,
		spaceID, k.Payee, k.Incoming, k.Currency)
	if err != nil {
		return false, fmt.Errorf("recurring: show: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}
