// Package structure lays the family's worth out (#400): by kind of asset, by
// currency, by account and by the country a paper was issued in, debts apart,
// in the base currency at today's rates. It is the family total of GET
// /summary taken apart — each account counted the way the total counts it,
// by its journal or by its last balance — and owns no table.
package structure

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// Class is a kind of asset.
type Class string

const (
	ClassShares   Class = "shares"
	ClassBonds    Class = "bonds"
	ClassFunds    Class = "funds"
	ClassCurrency Class = "currency"
	ClassMetals   Class = "metals"
	ClassCrypto   Class = "crypto"
	ClassOther    Class = "other"
	// ClassBrokerCash is the money on brokers' accounts.
	ClassBrokerCash Class = "broker_cash"
	// ClassMoney is cards, current accounts and cash.
	ClassMoney Class = "money"
	// ClassDeposits is savings accounts and deposits.
	ClassDeposits Class = "deposits"
	// ClassBrokerBalance is a broker's account counted by its balance mark:
	// what it holds is not known in parts.
	ClassBrokerBalance Class = "broker_balance"
)

// NoCountry stands for what was not issued anywhere: money, deposits, an
// account known only by its balance. UnknownCountry is a paper with no ISIN
// to tell its country by.
const (
	NoCountry      = ""
	UnknownCountry = "-"
)

// Valuation picks the worth a paper is struck at (decision Р-11).
type Valuation string

const (
	Liquid Valuation = "liquid"
	Full   Valuation = "full"
)

// Slice is one part of the worth: its key in the breakdown and what it comes
// to in the base currency.
type Slice struct {
	Key   string
	Minor int64
}

// Structure is the family's worth taken apart. Each breakdown adds up to
// Assets; Debts are apart, negative.
type Structure struct {
	BaseCurrency string
	Valuation    Valuation
	Assets       int64
	Debts        int64
	ByClass      []Slice
	ByCurrency   []Slice
	ByAccount    []Slice
	ByCountry    []Slice
	// Unpriced counts the papers left out for want of a price; MissingRates
	// the currencies left out for want of a rate.
	Unpriced     int
	MissingRates []string
}

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type positions interface {
	Positions(ctx context.Context, spaceID, accountID uuid.UUID) (apitypes.PositionsResponse, int, error)
}

type spaces interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// Service builds structures.
type Service struct {
	accounts  accounts
	positions positions
	spaces    spaces
	rates     marketdata.RateSource
}

func NewService(acc accounts, pos positions, sp spaces, rates marketdata.RateSource) *Service {
	return &Service{accounts: acc, positions: pos, spaces: sp, rates: rates}
}

// piece is one holding, before conversion.
type piece struct {
	class    Class
	currency string
	account  uuid.UUID
	country  string
	minor    int64
}

// Of is the space's structure today, papers at valuation.
func (s *Service) Of(ctx context.Context, spaceID uuid.UUID, valuation Valuation) (Structure, error) {
	if valuation != Liquid && valuation != Full {
		return Structure{}, fmt.Errorf("%w: valuation is liquid or full", family.ErrValidation)
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return Structure{}, err
	}
	out := Structure{BaseCurrency: sp.BaseCurrency, Valuation: valuation, MissingRates: []string{}}
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return Structure{}, err
	}
	var pieces []piece
	for _, a := range list {
		if a.Status != account.StatusActive {
			continue
		}
		got, byJournal, err := s.accountPieces(ctx, spaceID, a, valuation, &out)
		if err != nil {
			return Structure{}, err
		}
		if byJournal {
			pieces = append(pieces, got...)
			continue
		}
		if a.Balance == nil || a.Balance.AmountMinor == 0 {
			continue
		}
		pieces = append(pieces, piece{class: balanceClass(a.Type), currency: a.Currency, account: a.ID, country: NoCountry, minor: a.Balance.AmountMinor})
	}
	return out, s.lay(ctx, &out, pieces)
}

// accountPieces is an account counted by its journal, taken apart; byJournal
// is false when the total counts its balance instead.
func (s *Service) accountPieces(ctx context.Context, spaceID uuid.UUID, a account.WithBalance, valuation Valuation, out *Structure) ([]piece, bool, error) {
	if !account.CountedByJournal(a.Account) {
		return nil, false, nil
	}
	resp, ops, err := s.positions.Positions(ctx, spaceID, a.ID)
	if err != nil {
		return nil, false, err
	}
	if ops == 0 {
		return nil, false, nil
	}
	var got []piece
	for _, p := range resp.Positions {
		if p.Quantity == "0" {
			continue
		}
		value, currency := p.LiquidValueMinor, p.Currency
		if valuation == Full {
			value = p.MarketValueMinor
			if p.MarketValueCurrency.IsSpecified() && !p.MarketValueCurrency.IsNull() {
				currency = p.MarketValueCurrency.MustGet()
			}
		}
		if !value.IsSpecified() || value.IsNull() {
			out.Unpriced++
			continue
		}
		got = append(got, piece{
			class: paperClass(instrument.Type(p.Instrument.Type)), currency: currency, account: a.ID,
			country: issuerCountry(p.Instrument.Isin), minor: value.MustGet(),
		})
	}
	cashClass := ClassBrokerCash
	if a.Type != account.TypeBrokerage {
		cashClass = balanceClass(a.Type)
	}
	for _, c := range resp.Cash {
		if c.AmountMinor != 0 {
			got = append(got, piece{class: cashClass, currency: c.Currency, account: a.ID, country: NoCountry, minor: c.AmountMinor})
		}
	}
	return got, true, nil
}

// paperClass is the kind of asset a paper of type t is.
func paperClass(t instrument.Type) Class {
	switch t {
	case instrument.TypeShare:
		return ClassShares
	case instrument.TypeBond:
		return ClassBonds
	case instrument.TypeETF:
		return ClassFunds
	case instrument.TypeCurrency:
		return ClassCurrency
	case instrument.TypeMetal:
		return ClassMetals
	case instrument.TypeCrypto:
		return ClassCrypto
	}
	return ClassOther
}

// balanceClass is the kind of asset an account counted by its balance is.
func balanceClass(t account.Type) Class {
	switch t {
	case account.TypeBrokerage:
		return ClassBrokerBalance
	case account.TypeSavings, account.TypeDeposit:
		return ClassDeposits
	}
	return ClassMoney
}

// issuerCountry is the country an ISIN's first two letters name;
// UnknownCountry without one. An ISIN's prefix is where the paper was issued, which for a
// depositary receipt is the depositary's country, not the company's.
func issuerCountry(isin string) string {
	isin = strings.ToUpper(strings.TrimSpace(isin))
	if len(isin) != 12 {
		return UnknownCountry
	}
	return isin[:2]
}

// lay converts every piece into the base currency at today's rate and sums
// the breakdowns; a negative piece is a debt.
func (s *Service) lay(ctx context.Context, out *Structure, pieces []piece) error {
	memo := marketdata.NewRateMemo(s.rates)
	today := time.Now().UTC()
	missing := map[string]bool{}
	sums := map[string]map[string]int64{"class": {}, "currency": {}, "account": {}, "country": {}}
	for _, p := range pieces {
		inBase := p.minor
		if p.currency != out.BaseCurrency {
			res := memo.Rate(ctx, p.currency, out.BaseCurrency, today)
			if errors.Is(res.Err, marketdata.ErrNoRate) {
				missing[p.currency] = true
				continue
			}
			if res.Err != nil {
				return res.Err
			}
			converted, err := money.Minor(decimal.NewFromInt(p.minor).Mul(res.Rate))
			if err != nil {
				return fmt.Errorf("structure: %d %s in %s: %w", p.minor, p.currency, out.BaseCurrency, err)
			}
			inBase = converted
		}
		if inBase < 0 {
			out.Debts += inBase
			continue
		}
		out.Assets += inBase
		sums["class"][string(p.class)] += inBase
		sums["currency"][p.currency] += inBase
		sums["account"][p.account.String()] += inBase
		sums["country"][p.country] += inBase
	}
	for c := range missing {
		out.MissingRates = append(out.MissingRates, c)
	}
	slices.Sort(out.MissingRates)
	out.ByClass, out.ByCurrency = slicesOf(sums["class"]), slicesOf(sums["currency"])
	out.ByAccount, out.ByCountry = slicesOf(sums["account"]), slicesOf(sums["country"])
	return nil
}

// slicesOf is a breakdown, largest first.
func slicesOf(m map[string]int64) []Slice {
	out := make([]Slice, 0, len(m))
	for k, v := range m {
		if v != 0 {
			out = append(out, Slice{Key: k, Minor: v})
		}
	}
	slices.SortFunc(out, func(a, b Slice) int {
		if a.Minor != b.Minor {
			if a.Minor > b.Minor {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out
}
