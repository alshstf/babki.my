package portfolio

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/platform/money"
)

// YearMoves is an account's money and papers over a year, as the report on
// an account abroad lists them (decision Р-45, form КНД 1112520): each
// currency's cash at the start, what came in, what went out and the cash at
// the end; the papers' full worth by currency at the start and at the end.
// Unpriced counts the papers no price struck on either day.
type YearMoves struct {
	Cash        []CashMoves
	PapersStart map[string]int64
	PapersEnd   map[string]int64
	Unpriced    int
}

// CashMoves is one currency's money over the year. In and Out are every
// arrival and departure — deposits, sales, payments, purchases, fees, taxes
// — on the operations' own days; Start and End the balances at the ends of
// the year before and of this one.
type CashMoves struct {
	Currency            string
	Start, In, Out, End int64
}

// YearMoves reads the account's journal for year.
func (s *Service) YearMoves(ctx context.Context, spaceID, accountID uuid.UUID, year int) (YearMoves, error) {
	ops, err := s.ops.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return YearMoves{}, err
	}
	byCurrency := map[string]*CashMoves{}
	for _, o := range ops {
		if o.OccurredOn.Year() > year {
			continue
		}
		effect, moves, err := cashEffect(o)
		if err != nil {
			return YearMoves{}, journalDoesNotCompute{err}
		}
		if !moves {
			continue
		}
		m, ok := byCurrency[o.Currency]
		if !ok {
			m = &CashMoves{Currency: o.Currency}
			byCurrency[o.Currency] = m
		}
		switch {
		case o.OccurredOn.Year() < year:
			m.Start, err = money.Add(m.Start, effect)
		case effect > 0:
			m.In, err = money.Add(m.In, effect)
		default:
			m.Out, err = money.Add(m.Out, -effect)
		}
		if err != nil {
			return YearMoves{}, fmt.Errorf("the %s moves of account %s: %w", o.Currency, accountID, err)
		}
	}
	var out YearMoves
	for _, m := range byCurrency {
		m.End = m.Start + m.In - m.Out
		out.Cash = append(out.Cash, *m)
	}
	slices.SortFunc(out.Cash, func(a, b CashMoves) int { return strings.Compare(a.Currency, b.Currency) })
	start := time.Date(year-1, time.December, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC)
	values, err := s.ValuesOn(ctx, spaceID, accountID, []time.Time{start, end})
	if err != nil {
		return YearMoves{}, err
	}
	// The papers are the full worth less the money, currency by currency: the
	// worth on a day counts the same operations as the balances.
	out.PapersStart, out.PapersEnd = map[string]int64{}, map[string]int64{}
	for i, papers := range []map[string]int64{out.PapersStart, out.PapersEnd} {
		for c, minor := range values[i].FullByCurrency {
			papers[c] = minor
		}
		for _, m := range out.Cash {
			cash := m.Start
			if i == 1 {
				cash = m.End
			}
			papers[m.Currency] -= cash
			if papers[m.Currency] == 0 {
				delete(papers, m.Currency)
			}
		}
	}
	out.Unpriced = values[0].FullUnpriced + values[1].FullUnpriced
	return out, nil
}
