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

// Kind is a kind of asset the family's targets share the worth out by
// (decision Р-37): the classes, with foreign currency and metals held as
// money counted as such, and the base currency at brokers apart — money
// waiting to be put in.
type Kind string

const (
	KindShares          Kind = "shares"
	KindBonds           Kind = "bonds"
	KindFunds           Kind = "funds"
	KindForeignCurrency Kind = "foreign_currency"
	KindMetals          Kind = "metals"
	KindDeposits        Kind = "deposits"
	KindCrypto          Kind = "crypto"
	KindOther           Kind = "other"
	KindBrokerCash      Kind = "broker_cash"
	// kindEveryday is the base currency on cards, current accounts and in
	// cash — money to live on; kindUnsplit a broker's account counted by its
	// balance, whose kinds are not known. Neither is in the targets' worth.
	kindEveryday Kind = "everyday"
	kindUnsplit  Kind = "unsplit"
)

// Kinds are the kinds targets are set for, in the order they are shown.
var Kinds = []Kind{KindShares, KindBonds, KindFunds, KindForeignCurrency, KindMetals, KindDeposits, KindCrypto, KindOther, KindBrokerCash}

// metalCodes are the codes the brokers hold metals under as money.
var metalCodes = map[string]bool{"XAU": true, "XAG": true, "XPT": true, "XPD": true}

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
	// Targets is the worth the family's targets share out (decision Р-37):
	// the assets but the money to live on (Everyday) and the brokers'
	// accounts known only by their balance (Unsplit), by kind and by
	// currency.
	Targets           int64
	ByKind            []Slice
	ByTargetCurrency  []Slice
	Everyday, Unsplit int64
	// Unsellable is, by kind, what of the targets' worth cannot be sold now:
	// papers valued in full that have no price to sell at (decision Р-11).
	// Empty at the liquid valuation, which leaves them out.
	Unsellable []Slice
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

// piece is one holding, before conversion; unsellable, a paper valued in
// full with no price to sell it at now.
type piece struct {
	class      Class
	kind       Kind
	currency   string
	account    uuid.UUID
	country    string
	minor      int64
	unsellable bool
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
		got, byJournal, err := s.accountPieces(ctx, spaceID, a, sp.BaseCurrency, valuation, &out)
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
		pieces = append(pieces, piece{
			class: balanceClass(a.Type), kind: moneyKind(a.Type, a.Currency, sp.BaseCurrency, false),
			currency: a.Currency, account: a.ID, country: NoCountry, minor: a.Balance.AmountMinor,
		})
	}
	return out, s.lay(ctx, &out, pieces)
}

// accountPieces is an account counted by its journal, taken apart; byJournal
// is false when the total counts its balance instead.
func (s *Service) accountPieces(ctx context.Context, spaceID uuid.UUID, a account.WithBalance, base string, valuation Valuation, out *Structure) ([]piece, bool, error) {
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
		unsellable := !p.LiquidValueMinor.IsSpecified() || p.LiquidValueMinor.IsNull()
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
		class := paperClass(instrument.Type(p.Instrument.Type))
		got = append(got, piece{
			class: class, kind: paperKind(class), currency: currency, account: a.ID,
			country: issuerCountry(p.Instrument.Isin), minor: value.MustGet(), unsellable: unsellable,
		})
	}
	cashClass := ClassBrokerCash
	if a.Type != account.TypeBrokerage {
		cashClass = balanceClass(a.Type)
	}
	for _, c := range resp.Cash {
		if c.AmountMinor != 0 {
			got = append(got, piece{
				class: cashClass, kind: moneyKind(a.Type, c.Currency, base, true),
				currency: c.Currency, account: a.ID, country: NoCountry, minor: c.AmountMinor,
			})
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

// PaperKind is the kind a paper of type t is for the targets — as the
// structure counts it.
func PaperKind(t instrument.Type) Kind { return paperKind(paperClass(t)) }

// paperKind is the kind a paper of class c is for the targets.
func paperKind(c Class) Kind {
	switch c {
	case ClassShares:
		return KindShares
	case ClassBonds:
		return KindBonds
	case ClassFunds:
		return KindFunds
	case ClassCurrency:
		return KindForeignCurrency
	case ClassMetals:
		return KindMetals
	case ClassCrypto:
		return KindCrypto
	}
	return KindOther
}

// moneyKind is the kind money in currency on an account of type t is for the
// targets; byJournal says the journal, not a balance, holds it. Metals held
// as money are metals; deposits and savings are deposits in any currency; a
// broker's base currency is money waiting to be put in, its balance alone
// tells no kinds; other foreign currency is foreign currency — at a broker,
// on a current account or in cash; the base currency elsewhere is to live on.
func moneyKind(t account.Type, currency, base string, byJournal bool) Kind {
	switch {
	case metalCodes[currency]:
		return KindMetals
	case t == account.TypeSavings || t == account.TypeDeposit:
		return KindDeposits
	case t == account.TypeBrokerage && !byJournal:
		return kindUnsplit
	case t == account.TypeBrokerage && currency == base:
		return KindBrokerCash
	case currency != base:
		return KindForeignCurrency
	}
	return kindEveryday
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
	sums := map[string]map[string]int64{"class": {}, "currency": {}, "account": {}, "country": {}, "kind": {}, "targetCurrency": {}, "unsellable": {}}
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
		switch p.kind {
		case kindEveryday:
			out.Everyday += inBase
		case kindUnsplit:
			out.Unsplit += inBase
		default:
			out.Targets += inBase
			sums["kind"][string(p.kind)] += inBase
			sums["targetCurrency"][p.currency] += inBase
			if p.unsellable {
				sums["unsellable"][string(p.kind)] += inBase
			}
		}
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
	out.ByKind, out.ByTargetCurrency = slicesOf(sums["kind"]), slicesOf(sums["targetCurrency"])
	out.Unsellable = slicesOf(sums["unsellable"])
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
