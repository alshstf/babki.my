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
	"babki.my/babki/internal/intermediary"
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

// A paper's sector key is its instrument.Sector; NotPapers for what is not a
// paper, UnknownSector for a paper of no known sector.
const (
	NotPapers     = ""
	UnknownSector = "-"
)

// Where a paper is held (decision Р-46): a Russian paper at a Russian broker,
// a foreign one there — whose blocking is the risk — or anything at a broker
// abroad; NotAtBroker for what is not at a broker.
const (
	HeldDomestic      = "domestic"
	HeldForeignPaper  = "foreign_paper"
	HeldForeignBroker = "foreign_broker"
	NotAtBroker       = ""
)

// Valuation picks the worth a paper is struck at (decision Р-11).
type Valuation string

const (
	Liquid Valuation = "liquid"
	Full   Valuation = "full"
)

// Slice is one part of the worth: its key in the breakdown and what it comes
// to in the base currency. ByISIN, in the breakdown by country, is what of it
// has its country only by the ISIN, the broker not knowing the paper.
type Slice struct {
	Key    string
	Minor  int64
	ByISIN int64
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
	// ByCountry is by the country of risk (decision Р-46), BySector by the
	// sector, ByHolding by where a paper is held; Papers is what the papers
	// come to, AtBrokers what the brokers hold. ByBroker and ByBank are what
	// each broker and each bank holds (decision Р-47), by the catalog's id —
	// UnknownHolder for one the catalog does not know; AtBanks what the banks
	// hold: what is on cards, current and savings accounts and deposits.
	ByCountry         []Slice
	BySector          []Slice
	ByHolding         []Slice
	Papers, AtBrokers int64
	ByBroker, ByBank  []Slice
	AtBanks           int64
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

type worths interface {
	Worths(ctx context.Context, spaceID, accountID uuid.UUID, days []time.Time) ([]account.JournalValue, bool, error)
}

// Service builds structures.
type Service struct {
	accounts  accounts
	positions positions
	spaces    spaces
	rates     marketdata.RateSource
	valuables worths
	classes   classes
}

type classes interface {
	ClassesFor(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]instrument.Class, error)
}

// WithClasses lets the structure tell papers by their country of risk and
// sector (decision Р-46); without, by the ISIN's country, of no sector.
func (s *Service) WithClasses(c classes) *Service {
	s.classes = c
	return s
}

// WithValuables lets the structure take valuables accounts apart: their bars
// and coins are metals (decision Р-39).
func (s *Service) WithValuables(v worths) *Service {
	s.valuables = v
	return s
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
	// paper is the instrument a paper's piece is, nil for money; byISIN says
	// its country is the ISIN's; sector and held as the keys above.
	paper  *uuid.UUID
	byISIN bool
	sector string
	held   string
	// holder is the catalog's id of the bank or broker holding the account,
	// UnknownHolder when not known; holderKind which, empty for neither.
	holder     string
	holderKind intermediary.Kind
	// change is a planned change's piece: negative is less of what is held,
	// not a debt.
	change bool
}

// Change is a planned change to what an account holds, in Currency's minor
// units, signed: a paper (Paper set, with its type and ISIN) or money.
type Change struct {
	Account  uuid.UUID
	Paper    *uuid.UUID
	Type     instrument.Type
	ISIN     string
	Currency string
	Minor    int64
}

// Of is the space's structure today, papers at valuation.
func (s *Service) Of(ctx context.Context, spaceID uuid.UUID, valuation Valuation) (Structure, error) {
	return s.OfWith(ctx, spaceID, valuation, nil)
}

// OfWith is the structure as it would be after the changes (decision Р-48):
// each laid on top of what its account holds, as the account's own pieces
// would be. An account not of the space's, or archived, is refused.
func (s *Service) OfWith(ctx context.Context, spaceID uuid.UUID, valuation Valuation, changes []Change) (Structure, error) {
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
	for _, c := range changes {
		p, err := changePiece(list, c, sp.BaseCurrency)
		if err != nil {
			return Structure{}, err
		}
		pieces = append(pieces, p)
	}
	holders(list, pieces)
	if err := s.classify(ctx, spaceID, pieces); err != nil {
		return Structure{}, err
	}
	return out, s.lay(ctx, &out, pieces)
}

// UnknownHolder is a bank or broker the catalog does not know.
const UnknownHolder = "-"

// holders gives each piece the bank or broker holding its account: a broker
// for a brokerage account, a bank for any other but cash and valuables.
func holders(list []account.WithBalance, pieces []piece) {
	byID := map[uuid.UUID]account.Account{}
	for _, a := range list {
		byID[a.ID] = a.Account
	}
	for i := range pieces {
		a, ok := byID[pieces[i].account]
		if !ok || a.Type == account.TypeCash || a.Type == account.TypeValuables {
			continue
		}
		pieces[i].holderKind = intermediary.Bank
		if a.Type == account.TypeBrokerage {
			pieces[i].holderKind = intermediary.Broker
		}
		pieces[i].holder = UnknownHolder
		if found, _, ok := account.IntermediaryOf(a); ok {
			pieces[i].holder = found.ID
		}
	}
}

// classify gives each paper its country of risk and sector from its class;
// a paper with no country known keeps its ISIN's, marked so.
func (s *Service) classify(ctx context.Context, spaceID uuid.UUID, pieces []piece) error {
	ids := []uuid.UUID{}
	for _, p := range pieces {
		if p.paper != nil {
			ids = append(ids, *p.paper)
		}
	}
	known := map[uuid.UUID]instrument.Class{}
	if s.classes != nil && len(ids) > 0 {
		var err error
		if known, err = s.classes.ClassesFor(ctx, spaceID, ids); err != nil {
			return err
		}
	}
	for i := range pieces {
		p := &pieces[i]
		if p.paper == nil {
			p.sector = NotPapers
			continue
		}
		c := known[*p.paper]
		if c.Country != "" {
			p.country = c.Country
		} else {
			p.byISIN = p.country != UnknownCountry
		}
		p.sector = string(c.Sector)
		if p.sector == "" {
			p.sector = UnknownSector
		}
	}
	return nil
}

// accountPieces is an account counted by its journal, taken apart; byJournal
// is false when the total counts its balance instead.
func (s *Service) accountPieces(ctx context.Context, spaceID uuid.UUID, a account.WithBalance, base string, valuation Valuation, out *Structure) ([]piece, bool, error) {
	if !account.CountedByJournal(a.Account) {
		return nil, false, nil
	}
	if a.Type == account.TypeValuables {
		return s.valuablesPieces(ctx, spaceID, a, valuation, out)
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
		paper := p.Instrument.Id
		got = append(got, piece{
			class: class, kind: paperKind(class), currency: currency, account: a.ID,
			country: issuerCountry(p.Instrument.Isin), minor: value.MustGet(), unsellable: unsellable,
			paper: &paper, held: heldAt(a, p.Instrument.Isin),
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
				held: heldAt(a, ""),
			})
		}
	}
	return got, true, nil
}

// valuablesPieces are a valuables account's bars and coins: metals, in the
// currency the central bank prices them in, as they would sell now or by its
// price; a piece not valued yet is counted unpriced.
func (s *Service) valuablesPieces(ctx context.Context, spaceID uuid.UUID, a account.WithBalance, valuation Valuation, out *Structure) ([]piece, bool, error) {
	if s.valuables == nil {
		return nil, false, nil
	}
	got, ok, err := s.valuables.Worths(ctx, spaceID, a.ID, []time.Time{time.Now().UTC()})
	if err != nil || !ok || got[0].Operations == 0 {
		return nil, false, err
	}
	v := got[0]
	byCurrency, unpriced := v.ByCurrency, v.Unpriced
	if valuation == Full {
		byCurrency, unpriced = v.FullByCurrency, v.FullUnpriced
	}
	out.Unpriced += unpriced
	var pieces []piece
	for currency, minor := range byCurrency {
		if minor != 0 {
			pieces = append(pieces, piece{class: ClassMetals, kind: KindMetals, currency: currency, account: a.ID, country: NoCountry, minor: minor})
		}
	}
	return pieces, true, nil
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

// changePiece is a planned change as a piece of its account.
func changePiece(list []account.WithBalance, c Change, base string) (piece, error) {
	var a *account.WithBalance
	for i := range list {
		if list[i].ID == c.Account && list[i].Status == account.StatusActive {
			a = &list[i]
		}
	}
	if a == nil {
		return piece{}, fmt.Errorf("%w: no such active account", family.ErrValidation)
	}
	p := piece{currency: c.Currency, account: a.ID, country: NoCountry, minor: c.Minor, change: true}
	if c.Paper != nil {
		p.class = paperClass(c.Type)
		p.kind = paperKind(p.class)
		p.country = issuerCountry(c.ISIN)
		p.paper, p.held = c.Paper, heldAt(*a, c.ISIN)
		return p, nil
	}
	byJournal := account.CountedByJournal(a.Account)
	p.class = balanceClass(a.Type)
	if byJournal && a.Type == account.TypeBrokerage {
		p.class = ClassBrokerCash
	}
	p.kind = moneyKind(a.Type, c.Currency, base, byJournal)
	p.held = heldAt(*a, "")
	return p, nil
}

// heldAt is where a paper of this ISIN (money: none) on the account is held:
// anything at a broker abroad is there; at a Russian broker, a paper issued
// elsewhere is foreign; what is not at a broker is not.
func heldAt(a account.WithBalance, isin string) string {
	switch {
	case a.Type != account.TypeBrokerage:
		return NotAtBroker
	case a.ForeignBroker:
		return HeldForeignBroker
	case isin != "" && issuerCountry(isin) != "RU":
		return HeldForeignPaper
	}
	return HeldDomestic
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
	sums := map[string]map[string]int64{
		"class": {}, "currency": {}, "account": {}, "country": {}, "kind": {}, "targetCurrency": {}, "unsellable": {},
		"sector": {}, "holding": {}, "countryByISIN": {}, "broker": {}, "bank": {},
	}
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
		if inBase < 0 && !p.change {
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
		if p.byISIN {
			sums["countryByISIN"][p.country] += inBase
		}
		sums["sector"][p.sector] += inBase
		sums["holding"][p.held] += inBase
		if p.paper != nil {
			out.Papers += inBase
		}
		if p.held != NotAtBroker {
			out.AtBrokers += inBase
		}
		switch p.holderKind {
		case intermediary.Broker:
			sums["broker"][p.holder] += inBase
		case intermediary.Bank:
			sums["bank"][p.holder] += inBase
			out.AtBanks += inBase
		}
	}
	for c := range missing {
		out.MissingRates = append(out.MissingRates, c)
	}
	slices.Sort(out.MissingRates)
	out.ByClass, out.ByCurrency = slicesOf(sums["class"]), slicesOf(sums["currency"])
	out.ByAccount, out.ByCountry = slicesOf(sums["account"]), slicesOf(sums["country"])
	for i := range out.ByCountry {
		out.ByCountry[i].ByISIN = sums["countryByISIN"][out.ByCountry[i].Key]
	}
	out.BySector, out.ByHolding = slicesOf(sums["sector"]), slicesOf(sums["holding"])
	out.ByBroker, out.ByBank = slicesOf(sums["broker"]), slicesOf(sums["bank"])
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
