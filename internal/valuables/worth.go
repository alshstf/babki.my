package valuables

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

type spaces interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

type history interface {
	MetalPricesUpTo(ctx context.Context, metals []marketdata.Metal, to time.Time) ([]marketdata.MetalPrice, error)
}

// WithWorth lets the service tell a valuables account's worth in the base
// currency, as the family's total and its capital chart read an account's.
func (s *Service) WithWorth(sp spaces, h history, rates marketdata.RateSource) *Service {
	s.spaces, s.history, s.rates = sp, h, rates
	return s
}

// rouble is what the central bank prices metals in.
const rouble = "RUB"

// Worths is a valuables account's worth on each of days, as account.
// JournalValue carries an account's (decision Р-11): Minor what the pieces
// held that day would sell for, FullMinor what they are worth by the central
// bank's price, both in the base currency at that day's rate; Operations the
// pieces held. A piece with nothing to value it by is left out and counted
// (Unpriced, FullUnpriced). ok is false for an account of another type.
func (s *Service) Worths(ctx context.Context, spaceID, accountID uuid.UUID, days []time.Time) ([]account.JournalValue, bool, error) {
	a, err := s.accounts.ByID(ctx, spaceID, accountID)
	if err != nil {
		return nil, false, err
	}
	if a.Type != account.TypeValuables {
		return nil, false, nil
	}
	if s.spaces == nil {
		return nil, false, errors.New("valuables: worth asked of a service built without WithWorth")
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return nil, true, err
	}
	h, err := s.load(ctx, accountID)
	if err != nil {
		return nil, true, err
	}
	last := slices.MaxFunc(days, func(a, b time.Time) int { return a.Compare(b) })
	var metals []marketdata.Metal
	for _, it := range h.Items {
		if !slices.Contains(metals, it.Metal) {
			metals = append(metals, it.Metal)
		}
	}
	prices, err := s.history.MetalPricesUpTo(ctx, metals, last)
	if err != nil {
		return nil, true, err
	}
	rates := marketdata.NewRateMemo(s.rates)
	out := make([]account.JournalValue, len(days))
	for i, day := range days {
		v, err := worthOn(ctx, h, prices, sp.BaseCurrency, day, rates)
		if err != nil {
			return nil, true, err
		}
		out[i] = v
	}
	return out, true, nil
}

// priceOn is the metal's price on day or the nearest earlier one in prices
// (each metal's oldest first); nil when there is none.
func priceOn(prices []marketdata.MetalPrice, metal marketdata.Metal, day time.Time) *marketdata.MetalPrice {
	var found *marketdata.MetalPrice
	for i := range prices {
		p := &prices[i]
		if p.Metal != metal {
			continue
		}
		if p.On.After(day) {
			break
		}
		found = p
	}
	return found
}

func worthOn(ctx context.Context, h stored, prices []marketdata.MetalPrice, base string, day time.Time, rates *marketdata.RateMemo) (account.JournalValue, error) {
	v := account.JournalValue{
		Currency: base, ByCurrency: map[string]int64{}, FullByCurrency: map[string]int64{},
		MissingRates: []string{}, NegativeCash: []string{}, FullMissingRates: []string{},
	}
	var sell, market int64
	for _, it := range h.Items {
		if it.BoughtOn.After(day) {
			continue
		}
		v.Operations++
		val := Valuing(it, priceOn(prices, it.Metal, day), h.Storage, h.BankPrices, day)
		var err error
		if val.Sell == nil {
			v.Unpriced++
		} else if sell, err = money.Add(sell, *val.Sell); err != nil {
			return account.JournalValue{}, err
		}
		if val.Market == nil {
			v.FullUnpriced++
		} else if market, err = money.Add(market, *val.Market); err != nil {
			return account.JournalValue{}, err
		}
	}
	if v.Operations == 0 {
		return v, nil
	}
	v.ByCurrency[rouble], v.FullByCurrency[rouble] = sell, market
	if base == rouble {
		v.Minor, v.FullMinor = sell, market
		return v, nil
	}
	r := rates.Rate(ctx, rouble, base, day)
	if errors.Is(r.Err, marketdata.ErrNoRate) {
		v.MissingRates, v.FullMissingRates = []string{rouble}, []string{rouble}
		return v, nil
	}
	if r.Err != nil {
		return account.JournalValue{}, r.Err
	}
	var err error
	if v.Minor, err = money.Minor(decimal.NewFromInt(sell).Mul(r.Rate)); err != nil {
		return account.JournalValue{}, fmt.Errorf("valuables: %d roubles in %s: %w", sell, base, err)
	}
	if v.FullMinor, err = money.Minor(decimal.NewFromInt(market).Mul(r.Rate)); err != nil {
		return account.JournalValue{}, fmt.Errorf("valuables: %d roubles in %s: %w", market, base, err)
	}
	return v, nil
}
