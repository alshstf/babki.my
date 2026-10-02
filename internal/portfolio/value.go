package portfolio

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

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
	rates := make(map[rateKey]*rateLookup)
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
	return out, nil
}
