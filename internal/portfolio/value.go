package portfolio

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

// JournalValue is what a brokerage account is worth by its journal: every
// holding at its market value and the cash its operations leave, converted at
// today's rate into the space's base currency. It is the figure the family
// total takes for an account kept by its operations (the owner's ruling on
// Р-2, 2026-10-02), and it says alongside what it could not count.
//
// It is struck twice (decision Р-11): liquid — each holding at the price the
// market pays for it now, nothing when it does not trade — and full, each at
// its full price (see pricesOn). Both add the same cash.
type JournalValue struct {
	// Currency is the space's base currency, the one Minor is in.
	Currency string
	// Minor is the liquid worth, everything that could be valued, summed.
	Minor int64
	// ByCurrency is the same worth before conversion: each currency the
	// holdings and the cash are struck in, with what is held in it. Currencies
	// that come to nought are left out.
	ByCurrency map[string]int64
	// Operations is how many operations the journal holds. Nought means there
	// is nothing to value the account from.
	Operations int
	// Unpriced counts the holdings the liquid worth counts as nothing — no
	// market price now, a kind of paper this program has no model for.
	Unpriced int
	// NotTraded is how many of Unpriced the full worth still values.
	NotTraded int
	// MissingRates names the currencies held with no rate into Currency today.
	// What is held in them is left out of Minor.
	MissingRates []string
	// NegativeCash names the currencies whose cash by the journal is below
	// zero: money spent that the journal never saw arrive, usually a deposit
	// nobody recorded.
	NegativeCash []string
	// FullMinor, FullByCurrency, FullUnpriced and FullMissingRates are Minor,
	// ByCurrency, Unpriced and MissingRates of the full worth.
	FullMinor        int64
	FullByCurrency   map[string]int64
	FullUnpriced     int
	FullMissingRates []string
}

// worth adds holdings and cash by currency and converts each currency once.
type worth struct {
	byCurrency map[string]int64
	unpriced   int
}

func newWorth() *worth { return &worth{byCurrency: map[string]int64{}} }

func (w *worth) add(currency string, minor int64) error {
	sum, err := money.Add(w.byCurrency[currency], minor)
	if err != nil {
		return fmt.Errorf("%w: adding %d to %d in %s", err, minor, w.byCurrency[currency], currency)
	}
	w.byCurrency[currency] = sum
	return nil
}

// struck converts each currency at day's rate into base: the sum, the
// currencies with no rate (left out), and the non-zero currencies.
func (h *Handler) struck(ctx context.Context, w *worth, base string, day time.Time) (int64, []string, map[string]int64, error) {
	rates := marketdata.NewRateMemo(h.conv)
	currencies := make([]string, 0, len(w.byCurrency))
	for currency := range w.byCurrency {
		currencies = append(currencies, currency)
	}
	slices.Sort(currencies)
	var (
		total   int64
		missing []string
		err     error
	)
	nonZero := map[string]int64{}
	for _, currency := range currencies {
		minor := w.byCurrency[currency]
		if minor == 0 {
			continue
		}
		nonZero[currency] = minor
		if currency != base {
			converted, ok, err := h.sumInBase(ctx, []datedMinor{{minor: minor, from: currency, on: day}}, base, rates)
			if err != nil {
				return 0, nil, nil, err
			}
			if !ok {
				missing = append(missing, currency)
				continue
			}
			minor = converted
		}
		if total, err = money.Add(total, minor); err != nil {
			return 0, nil, nil, err
		}
	}
	return total, missing, nonZero, nil
}

// finish strikes both worths into v.
func (h *Handler) finish(ctx context.Context, v *JournalValue, liquid, full *worth, day time.Time) error {
	var err error
	if v.Minor, v.MissingRates, v.ByCurrency, err = h.struck(ctx, liquid, v.Currency, day); err != nil {
		return err
	}
	if v.FullMinor, v.FullMissingRates, v.FullByCurrency, err = h.struck(ctx, full, v.Currency, day); err != nil {
		return err
	}
	v.Unpriced, v.FullUnpriced = liquid.unpriced, full.unpriced
	slices.Sort(v.NegativeCash)
	return nil
}

// ValueFromJournal values one account from its journal, by the very figures its
// positions screen shows (see positionsResponse): a closed position adds
// nothing, an open one its market value in whatever currency that is struck
// in, and each currency's cash its balance. Each currency is then converted
// once, at today's rate.
func (h *Handler) ValueFromJournal(ctx context.Context, spaceID, accountID uuid.UUID) (JournalValue, error) {
	resp, operations, err := h.positionsResponse(ctx, spaceID, accountID)
	if err != nil {
		return JournalValue{}, err
	}
	out := JournalValue{Currency: resp.AccountTotal.BaseCurrency, Operations: operations}
	liquid, full := newWorth(), newWorth()
	for _, p := range resp.Positions {
		if p.Quantity == "0" {
			continue
		}
		if p.MarketValueMinor.IsNull() || !p.MarketValueMinor.IsSpecified() {
			full.unpriced++
		} else if err := full.add(p.MarketValueCurrency.MustGet(), p.MarketValueMinor.MustGet()); err != nil {
			return JournalValue{}, fmt.Errorf("the full value of account %s: %w", accountID, err)
		}
		if p.LiquidValueMinor.IsNull() || !p.LiquidValueMinor.IsSpecified() {
			liquid.unpriced++
			if !p.MarketValueMinor.IsNull() && p.MarketValueMinor.IsSpecified() {
				out.NotTraded++
			}
		} else if err := liquid.add(p.Currency, p.LiquidValueMinor.MustGet()); err != nil {
			return JournalValue{}, fmt.Errorf("the value of account %s: %w", accountID, err)
		}
	}
	for _, c := range resp.Cash {
		if c.AmountMinor < 0 {
			out.NegativeCash = append(out.NegativeCash, c.Currency)
		}
		if err := liquid.add(c.Currency, c.AmountMinor); err != nil {
			return JournalValue{}, fmt.Errorf("the value of account %s: %w", accountID, err)
		}
		if err := full.add(c.Currency, c.AmountMinor); err != nil {
			return JournalValue{}, fmt.Errorf("the full value of account %s: %w", accountID, err)
		}
	}
	if err := h.finish(ctx, &out, liquid, full, time.Now().UTC()); err != nil {
		return JournalValue{}, fmt.Errorf("the value of account %s: %w", accountID, err)
	}
	return out, nil
}

// ErrNoQuoteHistory is a quote store that keeps no past prices.
var ErrNoQuoteHistory = errors.New("portfolio: the quote store keeps no past prices")

// ValueOn values an account from its journal as it stood at the end of day:
// the operations up to that day, each holding at its liquid and full prices of
// that day (see pricesOn), each currency converted at that day's rate.
func (h *Handler) ValueOn(ctx context.Context, spaceID, accountID uuid.UUID, day time.Time) (JournalValue, error) {
	values, err := h.ValuesOn(ctx, spaceID, accountID, []time.Time{day})
	if err != nil {
		return JournalValue{}, err
	}
	return values[0], nil
}

// ValuesOn is ValueOn for several days, the journal read once.
func (h *Handler) ValuesOn(ctx context.Context, spaceID, accountID uuid.UUID, days []time.Time) ([]JournalValue, error) {
	if _, ok := h.quotes.(priceStore); !ok {
		return nil, ErrNoQuoteHistory
	}
	sp, err := h.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	all, err := h.ops.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]JournalValue, 0, len(days))
	for _, day := range days {
		v, err := h.valueOn(ctx, sp.BaseCurrency, sp.FullValuation, accountID, all, day)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// valueOn values the journal all as it stood at the end of day.
func (h *Handler) valueOn(ctx context.Context, base string, setting family.FullValuation, accountID uuid.UUID, all []Operation, day time.Time) (JournalValue, error) {
	var ops []Operation
	for _, o := range all {
		if !o.OccurredOn.After(day) {
			ops = append(ops, o)
		}
	}
	out := JournalValue{Currency: base, Operations: len(ops)}
	if len(ops) == 0 {
		out.ByCurrency, out.FullByCurrency = map[string]int64{}, map[string]int64{}
		return out, nil
	}
	positions, err := Compute(ops)
	if err != nil {
		return JournalValue{}, journalDoesNotCompute{err}
	}
	cash, err := Cash(ops)
	if err != nil {
		return JournalValue{}, journalDoesNotCompute{err}
	}

	ids := make([]uuid.UUID, 0, len(positions))
	for id, p := range positions {
		if p.Quantity.IsPositive() {
			ids = append(ids, id)
		}
	}
	papers, err := h.instruments.ByIDs(ctx, ids)
	if err != nil {
		return JournalValue{}, err
	}
	book, err := h.pricesOn(ctx, ids, day, setting, pastWindows)
	if err != nil {
		return JournalValue{}, err
	}

	liquid, full := newWorth(), newWorth()
	for _, id := range ids {
		paper, ok := papers[id]
		if !ok {
			return JournalValue{}, errInstrumentNotInCatalog
		}
		fq, fullQuoted := book.full[id]
		minor, currency, gap, err := marketValue(paper.Type, paper.FaceValueMinor, paper.FaceCurrency, positions[id].Quantity, fq.Quote, fullQuoted)
		if err != nil {
			return JournalValue{}, err
		}
		fullStruck := gap == valuationStruck
		if !fullStruck {
			full.unpriced++
		} else if err := full.add(currency, minor); err != nil {
			return JournalValue{}, fmt.Errorf("the full value of account %s on %s: %w", accountID, day.Format(time.DateOnly), err)
		}
		lq, liquidQuoted := book.liquid[id]
		minor, currency, gap, err = marketValue(paper.Type, paper.FaceValueMinor, paper.FaceCurrency, positions[id].Quantity, lq, liquidQuoted)
		if err != nil {
			return JournalValue{}, err
		}
		if gap != valuationStruck {
			liquid.unpriced++
			if fullStruck {
				out.NotTraded++
			}
			continue
		}
		if err := liquid.add(currency, minor); err != nil {
			return JournalValue{}, fmt.Errorf("the value of account %s on %s: %w", accountID, day.Format(time.DateOnly), err)
		}
	}
	for _, c := range CashByCurrency(cash) {
		if c.Minor < 0 {
			out.NegativeCash = append(out.NegativeCash, c.Currency)
		}
		if err := liquid.add(c.Currency, c.Minor); err != nil {
			return JournalValue{}, fmt.Errorf("the value of account %s on %s: %w", accountID, day.Format(time.DateOnly), err)
		}
		if err := full.add(c.Currency, c.Minor); err != nil {
			return JournalValue{}, fmt.Errorf("the full value of account %s on %s: %w", accountID, day.Format(time.DateOnly), err)
		}
	}
	if err := h.finish(ctx, &out, liquid, full, day); err != nil {
		return JournalValue{}, fmt.Errorf("the value of account %s on %s: %w", accountID, day.Format(time.DateOnly), err)
	}
	return out, nil
}
