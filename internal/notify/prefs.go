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
)

// Kinds are every kind, in the order the settings show them.
var Kinds = []Kind{KindCard, KindBudget, KindGoal, KindStorage, KindPayout, KindDeclared, KindSync, KindReconcile, KindStale}

// Prefs are a member's choice: the kinds turned off, and the hours from
// QuietFrom to QuietTo, by the server's clock, when nothing is sent.
type Prefs struct {
	Off                []Kind
	QuietFrom, QuietTo int
}

// DefaultPrefs: every kind, quiet from 21 to 9 — as reminders went before
// there was a choice.
func DefaultPrefs() Prefs { return Prefs{QuietFrom: 21, QuietTo: 9} }

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

// Wants says the member takes reminders of kind.
func (p Prefs) Wants(k Kind) bool { return !slices.Contains(p.Off, k) }

var errPrefs = fmt.Errorf("%w: quiet hours are 0 to 23, and the kinds are the known ones", family.ErrValidation)

// Prefs are the member's choice, the default without one.
func (s *Store) Prefs(ctx context.Context, userID uuid.UUID) (Prefs, error) {
	p := DefaultPrefs()
	var off []string
	err := s.db.QueryRow(ctx, `SELECT off, quiet_from, quiet_to FROM push_prefs WHERE user_id = $1`, userID).Scan(&off, &p.QuietFrom, &p.QuietTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return Prefs{}, fmt.Errorf("notify: prefs: %w", err)
	}
	for _, k := range off {
		p.Off = append(p.Off, Kind(k))
	}
	return p, nil
}

// SetPrefs keeps the member's choice.
func (s *Store) SetPrefs(ctx context.Context, userID uuid.UUID, p Prefs) error {
	if p.QuietFrom < 0 || p.QuietFrom > 23 || p.QuietTo < 0 || p.QuietTo > 23 {
		return errPrefs
	}
	off := make([]string, 0, len(p.Off))
	for _, k := range p.Off {
		if !slices.Contains(Kinds, k) {
			return errPrefs
		}
		if !slices.Contains(off, string(k)) {
			off = append(off, string(k))
		}
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO push_prefs (user_id, off, quiet_from, quiet_to) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE SET off = EXCLUDED.off, quiet_from = EXCLUDED.quiet_from, quiet_to = EXCLUDED.quiet_to`,
		userID, off, p.QuietFrom, p.QuietTo)
	if err != nil {
		return fmt.Errorf("notify: set prefs: %w", err)
	}
	return nil
}
