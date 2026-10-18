package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
)

// Kind is what a reminder is about; a member may turn each kind off
// (decision Р-61 Б).
type Kind string

const (
	KindCard      Kind = "card"
	KindBudget    Kind = "budget"
	KindStorage   Kind = "storage"
	KindGoal      Kind = "goal"
	KindSync      Kind = "sync"
	KindReconcile Kind = "reconcile"
	KindStale     Kind = "stale"
	KindPayout    Kind = "payout"
	KindDeclared  Kind = "declared"
	KindLoyalty   Kind = "loyalty"
)

// Kinds are every kind, in the order the settings show them.
var Kinds = []Kind{KindCard, KindLoyalty, KindBudget, KindGoal, KindStorage, KindPayout, KindDeclared, KindSync, KindReconcile, KindStale}

// Prefs are a member's choice: the kinds turned off on the devices and in
// Telegram, whether Telegram's messages carry the sums, and the hours from
// QuietFrom to QuietTo, by the server's clock, when nothing is sent.
type Prefs struct {
	Off, TelegramOff   []Kind
	Amounts            bool
	QuietFrom, QuietTo int
}

// DefaultPrefs: every kind, the sums in, quiet from 21 to 9 — as reminders
// went before there was a choice.
func DefaultPrefs() Prefs { return Prefs{Amounts: true, QuietFrom: 21, QuietTo: 9} }

// Quiet says nothing is sent at hour; equal hours are never quiet.
func (p Prefs) Quiet(hour int) bool {
	switch {
	case p.QuietFrom == p.QuietTo:
		return false
	case p.QuietFrom < p.QuietTo:
		return hour >= p.QuietFrom && hour < p.QuietTo
	default:
		return hour >= p.QuietFrom || hour < p.QuietTo
	}
}

// Wants says the member takes reminders of kind on their devices.
func (p Prefs) Wants(k Kind) bool { return !slices.Contains(p.Off, k) }

// WantsTelegram says the member takes reminders of kind in Telegram.
func (p Prefs) WantsTelegram(k Kind) bool { return !slices.Contains(p.TelegramOff, k) }

var errPrefs = fmt.Errorf("%w: quiet hours are 0 to 23, and the kinds are the known ones", family.ErrValidation)

// Prefs are the member's choice, the default without one.
func (s *Store) Prefs(ctx context.Context, userID uuid.UUID) (Prefs, error) {
	p := DefaultPrefs()
	var off, tg []string
	err := s.db.QueryRow(ctx, `SELECT off, telegram_off, amounts, quiet_from, quiet_to FROM push_prefs WHERE user_id = $1`, userID).
		Scan(&off, &tg, &p.Amounts, &p.QuietFrom, &p.QuietTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return Prefs{}, fmt.Errorf("notify: prefs: %w", err)
	}
	for _, k := range off {
		p.Off = append(p.Off, Kind(k))
	}
	for _, k := range tg {
		p.TelegramOff = append(p.TelegramOff, Kind(k))
	}
	return p, nil
}

// SetPrefs keeps the member's choice.
func (s *Store) SetPrefs(ctx context.Context, userID uuid.UUID, p Prefs) error {
	if p.QuietFrom < 0 || p.QuietFrom > 23 || p.QuietTo < 0 || p.QuietTo > 23 {
		return errPrefs
	}
	off, err := kindsOf(p.Off)
	if err != nil {
		return err
	}
	tg, err := kindsOf(p.TelegramOff)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO push_prefs (user_id, off, telegram_off, amounts, quiet_from, quiet_to) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id) DO UPDATE SET off = EXCLUDED.off, telegram_off = EXCLUDED.telegram_off, amounts = EXCLUDED.amounts,
			quiet_from = EXCLUDED.quiet_from, quiet_to = EXCLUDED.quiet_to`,
		userID, off, tg, p.Amounts, p.QuietFrom, p.QuietTo)
	if err != nil {
		return fmt.Errorf("notify: set prefs: %w", err)
	}
	return nil
}

// kindsOf are the kinds as kept, each once; an unknown one is refused.
func kindsOf(list []Kind) ([]string, error) {
	out := make([]string, 0, len(list))
	for _, k := range list {
		if !slices.Contains(Kinds, k) {
			return nil, errPrefs
		}
		if !slices.Contains(out, string(k)) {
			out = append(out, string(k))
		}
	}
	return out, nil
}
