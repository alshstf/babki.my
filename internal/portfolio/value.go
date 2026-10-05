package portfolio

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

// JournalValue is what a brokerage account is worth by its journal: every
// holding at its market value and the cash its operations leave, converted at
// today's rate into the space's base currency. It is the figure the family
// total takes for an account kept by its operations (the owner's ruling on
// Р-2, 2026-10-02), and it says alongside what it could not count.
type JournalValue struct {
	// Currency is the space's base currency, the one Minor is in.
	Currency string
	// Minor is everything that could be valued, summed.
	Minor int64
	// ByCurrency is the same worth before conversion: each currency the
	// holdings and the cash are struck in, with what is held in it. Currencies
	// that come to nought are left out.
	ByCurrency map[string]int64
	// Operations is how many operations the journal holds. Nought means there
	// is nothing to value the account from.
	Operations int
	// Unpriced counts the holdings with no valuation at all — no quote, a kind
	// of paper this program has no model for — which add nothing to Minor, the
	// same nought the positions screen counts them at.
	Unpriced int
	// MissingRates names the currencies held with no rate into Currency today.
	// What is held in them is left out of Minor.
	MissingRates []string
	// NegativeCash names the currencies whose cash by the journal is below
	// zero: money spent that the journal never saw arrive, usually a deposit
	// nobody recorded.
	NegativeCash []string
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

	byCurrency := map[string]int64{}
	add := func(currency string, minor int64) error {
		sum, err := money.Add(byCurrency[currency], minor)
		if err != nil {
			return fmt.Errorf("%w: the value of account %s in %s, adding %d to %d",
				err, accountID, currency, minor, byCurrency[currency])
		}
		byCurrency[currency] = sum
		return nil
	}
	for _, p := range resp.Positions {
		if p.Quantity == "0" {
			continue
		}
		if p.MarketValueMinor.IsNull() || !p.MarketValueMinor.IsSpecified() {
			out.Unpriced++
			continue
		}
		if err := add(p.MarketValueCurrency.MustGet(), p.MarketValueMinor.MustGet()); err != nil {
			return JournalValue{}, err
		}
	}
	for _, c := range resp.Cash {
		if c.AmountMinor < 0 {
			out.NegativeCash = append(out.NegativeCash, c.Currency)
		}
		if err := add(c.Currency, c.AmountMinor); err != nil {
			return JournalValue{}, err
		}
	}

	now := time.Now().UTC()
	rates := marketdata.NewRateMemo(h.conv)
	currencies := make([]string, 0, len(byCurrency))
	for currency := range byCurrency {
		currencies = append(currencies, currency)
	}
	slices.Sort(currencies)
	for _, currency := range currencies {
		minor := byCurrency[currency]
		if currency != out.Currency && minor != 0 {
			converted, ok, err := h.sumInBase(ctx, []datedMinor{{minor: minor, from: currency, on: now}}, out.Currency, rates)
			if err != nil {
				return JournalValue{}, err
			}
			if !ok {
				out.MissingRates = append(out.MissingRates, currency)
				continue
			}
			minor = converted
		}
		if out.Minor, err = money.Add(out.Minor, minor); err != nil {
			return JournalValue{}, fmt.Errorf("%w: the value of account %s in %s", err, accountID, out.Currency)
		}
	}
	slices.Sort(out.NegativeCash)
	out.ByCurrency = make(map[string]int64, len(byCurrency))
	for currency, minor := range byCurrency {
		if minor != 0 {
			out.ByCurrency[currency] = minor
		}
	}
	return out, nil
}

// quoteHistory is the price store as a past day needs it.
type quoteHistory interface {
	QuotesOn(ctx context.Context, instrumentIDs []uuid.UUID, day time.Time) (map[uuid.UUID]marketdata.Quote, error)
}

// staleQuoteDays is how much older than the day valued a closing price may be
// and still value it: a long weekend or the New Year holidays, not a paper
// whose prices stopped.
const staleQuoteDays = 10

// ErrNoQuoteHistory is a quote store that keeps no past prices.
var ErrNoQuoteHistory = errors.New("portfolio: the quote store keeps no past prices")

// ValueOn values an account from its journal as it stood at the end of day:
// the operations up to that day, each holding at its closing price of that day
// (or of a day at most staleQuoteDays earlier), each currency converted at that
// day's rate. Holdings with no such price are Unpriced and add nothing.
func (h *Handler) ValueOn(ctx context.Context, spaceID, accountID uuid.UUID, day time.Time) (JournalValue, error) {
	values, err := h.ValuesOn(ctx, spaceID, accountID, []time.Time{day})
	if err != nil {
		return JournalValue{}, err
	}
	return values[0], nil
}

// ValuesOn is ValueOn for several days, the journal read once.
func (h *Handler) ValuesOn(ctx context.Context, spaceID, accountID uuid.UUID, days []time.Time) ([]JournalValue, error) {
	history, ok := h.quotes.(quoteHistory)
	if !ok {
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
		v, err := h.valueOn(ctx, history, sp.BaseCurrency, accountID, all, day)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// valueOn values the journal all as it stood at the end of day.
func (h *Handler) valueOn(ctx context.Context, history quoteHistory, base string, accountID uuid.UUID, all []Operation, day time.Time) (JournalValue, error) {
	var ops []Operation
	for _, o := range all {
		if !o.OccurredOn.After(day) {
			ops = append(ops, o)
		}
	}
	out := JournalValue{Currency: base, Operations: len(ops)}
	if len(ops) == 0 {
		out.ByCurrency = map[string]int64{}
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
	quotes, err := history.QuotesOn(ctx, ids, day)
	if err != nil {
		return JournalValue{}, err
	}

	byCurrency := map[string]int64{}
	add := func(currency string, minor int64) error {
		sum, err := money.Add(byCurrency[currency], minor)
		if err != nil {
			return fmt.Errorf("%w: the value of account %s on %s in %s", err, accountID, day.Format(time.DateOnly), currency)
		}
		byCurrency[currency] = sum
		return nil
	}
	for _, id := range ids {
		paper, ok := papers[id]
		if !ok {
			return JournalValue{}, errInstrumentNotInCatalog
		}
		q, quoted := quotes[id]
		if quoted && q.On.Before(day.AddDate(0, 0, -staleQuoteDays)) {
			quoted = false
		}
		minor, currency, gap, err := marketValue(paper.Type, paper.FaceValueMinor, paper.FaceCurrency, positions[id].Quantity, q, quoted)
		if err != nil {
			return JournalValue{}, err
		}
		if gap != valuationStruck {
			out.Unpriced++
			continue
		}
		if err := add(currency, minor); err != nil {
			return JournalValue{}, err
		}
	}
	for _, c := range CashByCurrency(cash) {
		if c.Minor < 0 {
			out.NegativeCash = append(out.NegativeCash, c.Currency)
		}
		if err := add(c.Currency, c.Minor); err != nil {
			return JournalValue{}, err
		}
	}

	rates := marketdata.NewRateMemo(h.conv)
	currencies := make([]string, 0, len(byCurrency))
	for currency := range byCurrency {
		currencies = append(currencies, currency)
	}
	slices.Sort(currencies)
	out.ByCurrency = map[string]int64{}
	for _, currency := range currencies {
		minor := byCurrency[currency]
		if minor != 0 {
			out.ByCurrency[currency] = minor
		}
		if currency != out.Currency && minor != 0 {
			converted, ok, err := h.sumInBase(ctx, []datedMinor{{minor: minor, from: currency, on: day}}, out.Currency, rates)
			if err != nil {
				return JournalValue{}, err
			}
			if !ok {
				out.MissingRates = append(out.MissingRates, currency)
				continue
			}
			minor = converted
		}
		if out.Minor, err = money.Add(out.Minor, minor); err != nil {
			return JournalValue{}, fmt.Errorf("%w: the value of account %s on %s", err, accountID, day.Format(time.DateOnly))
		}
	}
	slices.Sort(out.NegativeCash)
	return out, nil
}
