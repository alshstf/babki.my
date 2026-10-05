package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

// MaxCapitalPoints is the most days one request values.
const MaxCapitalPoints = 120

// CapitalPoint is what the family's active accounts were worth at the end of
// one day, in the base currency, each counted the way the total counts it
// today: a brokerage account kept by its operations by its journal as it then
// stood, every other account by its latest balance mark on or before the day.
type CapitalPoint struct {
	Day      time.Time
	Minor    int64
	Complete bool
	Accounts []AccountWorth
}

// AccountWorth is one account's part of a CapitalPoint. Complete is false when
// something in it could not be valued that day — a paper with no price, a
// currency with no rate — and is then left out.
type AccountWorth struct {
	AccountID uuid.UUID
	Minor     int64
	ByJournal bool
	Complete  bool
}

// capital values the space's active accounts on each of days.
func (h *Handler) capital(ctx context.Context, spaceID uuid.UUID, base string, days []time.Time) ([]CapitalPoint, error) {
	accounts, err := h.store.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	marks, err := h.store.BalanceHistory(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	vals, err := h.valuations(ctx, spaceID, accounts, base, time.Now().UTC(), marketdata.NewRateMemo(h.converter))
	if err != nil {
		return nil, err
	}
	points := make([]CapitalPoint, len(days))
	for i, day := range days {
		points[i] = CapitalPoint{Day: day, Complete: true}
	}
	for _, a := range accounts {
		if a.Status != StatusActive {
			continue
		}
		var journal []JournalValue
		if v, ok := vals[a.ID]; ok && v.byJournal {
			if journal, err = h.journals.ValuesOn(ctx, spaceID, a.ID, days); err != nil {
				return nil, fmt.Errorf("value account %s by day: %w", a.ID, err)
			}
		}
		for i, day := range days {
			worth := AccountWorth{AccountID: a.ID, Complete: true}
			if journal != nil && journal[i].Operations > 0 {
				j := journal[i]
				worth.Minor, worth.ByJournal = j.Minor, true
				worth.Complete = j.Unpriced == 0 && len(j.MissingRates) == 0
			} else if mark, ok := markOn(marks[a.ID], day); ok {
				minor, converted, err := h.inBaseOn(ctx, mark, a.Currency, base, day)
				if err != nil {
					return nil, err
				}
				worth.Minor, worth.Complete = minor, converted
			}
			p := &points[i]
			p.Accounts = append(p.Accounts, worth)
			if !worth.Complete {
				p.Complete = false
			}
			if p.Minor, err = money.Add(p.Minor, worth.Minor); err != nil {
				return nil, fmt.Errorf("%w: the family's worth on %s", err, day.Format(time.DateOnly))
			}
		}
	}
	return points, nil
}

// markOn is the latest of marks dated on or before day.
func markOn(marks []BalancePoint, day time.Time) (int64, bool) {
	var (
		minor int64
		found bool
	)
	for _, m := range marks {
		if m.AsOf.After(day) {
			break
		}
		minor, found = m.AmountMinor, true
	}
	return minor, found
}

// inBaseOn puts an amount of currency into base at day's rate; converted is
// false when there is no rate, and the amount then counts as nothing.
func (h *Handler) inBaseOn(ctx context.Context, minor int64, currency, base string, day time.Time) (int64, bool, error) {
	if currency == base || minor == 0 {
		return minor, true, nil
	}
	rate, _, err := h.converter.Rate(ctx, currency, base, day)
	if errors.Is(err, marketdata.ErrNoRate) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	v, err := money.Minor(decimal.NewFromInt(minor).Mul(rate))
	if err != nil {
		return 0, false, fmt.Errorf("%w: a balance of %d %s on %s", err, minor, currency, day.Format(time.DateOnly))
	}
	return v, true, nil
}

// capitalDays are the days a series from from is valued on: every step after
// from that has passed, and today.
func capitalDays(from, today time.Time, monthly bool) ([]time.Time, error) {
	if from.After(today) {
		return nil, fmt.Errorf("from is after today")
	}
	var days []time.Time
	if monthly {
		// The last day of every month from from's on, before today's month.
		end := time.Date(from.Year(), from.Month()+1, 0, 0, 0, 0, 0, time.UTC)
		for end.Before(today) {
			days = append(days, end)
			end = time.Date(end.Year(), end.Month()+2, 0, 0, 0, 0, 0, time.UTC)
		}
	} else {
		for d := from; d.Before(today); d = d.AddDate(0, 0, 7) {
			days = append(days, d)
		}
	}
	days = append(days, today)
	if len(days) > MaxCapitalPoints {
		return nil, fmt.Errorf("at most %d points; choose a later start or monthly steps", MaxCapitalPoints)
	}
	return days, nil
}
