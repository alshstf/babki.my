package taxyear

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/portfolio"
)

type rows interface {
	YearRows(ctx context.Context, spaceID, accountID uuid.UUID, year int, currency string, longYears int) (portfolio.YearRows, error)
}

type papers interface {
	ByID(ctx context.Context, id uuid.UUID) (instrument.Instrument, error)
}

// WithForeign lets the service lay out the year of the accounts at brokers
// abroad line by line, for the tax return (decision Р-45).
func (s *Service) WithForeign(r rows, p papers) *Service {
	s.rows, s.papers = r, p
	return s
}

// Income kinds of the return's Приложение 2, line 031 (приложение № 4 к
// Порядку заполнения 3-НДФЛ, приказ ФНС ЕД-7-11/913@): dividends; operations
// with securities — the sales and, as the Ministry of Finance reads ст. 214.1,
// the coupons; interest from sources abroad.
const (
	KindDividends  = "001"
	KindSecurities = "003"
	KindInterest   = "013"
)

// Foreign is the year of each account at a broker abroad as the tax return
// lists it (decision Р-45, step 1). An estimate from the journal, like the
// rest: the family checks it against the broker's report.
type Foreign struct {
	Year     int
	Currency string
	// TreatiesAsOf is the day of the ministry's list the credit is judged by.
	TreatiesAsOf string
	Accounts     []ForeignAccount
}

// ForeignAccount is one account's lines; Taxes the tax rows set against no
// payment. Country is the broker's, the
// country the money is credited in (line 011), empty when the program does
// not know the broker. Income and Expenses are Приложение 8's lines 020 and
// 030 for its sales; Unvalued counts the lines a rate or a lot's day is
// missing for, left out of them.
type ForeignAccount struct {
	ID       uuid.UUID
	Name     string
	Person   uuid.UUID
	Country  string
	Payouts  []ForeignPayout
	Sales    []ForeignSale
	Taxes    []ForeignPayout
	Income   int64
	Expenses int64
	Unvalued int
}

// ForeignPayout is a payment's line of Приложение 2: its income kind, the
// paper and the country of its issuer as the ISIN tells it (line 010), the
// Russian tax on it at the first band's rate (line 160) and what the tax
// withheld abroad offsets of it (line 170), no more than that tax and only
// where the residency's treaty with that country applies on the payment's day
// (decision Р-50): Treaty is its state, TreatyFrom the day that state began.
type ForeignPayout struct {
	portfolio.Payout
	Kind       string
	Paper      string
	ISIN       string
	Country    string
	TaxRU      int64
	Credit     int64
	Treaty     string
	TreatyFrom *time.Time
}

// A payment's treaty when no state of the residency's book fits: the paper
// names no country (no ISIN, or an international one), so the treaty cannot
// be told.
const TreatyNoCountry = "no_country"

// credit is what a payment's tax withheld abroad offsets: the Russian tax on it
// at the first band, the withheld no more than that, where the treaty with
// the issuer's country applies on its day. Without the residency's treaties
// in the application, the treaty is not judged.
func credit(p portfolio.Payout, country string, rules family.TaxRules, book family.Treaties, haveBook bool) (taxRU, credited int64, state string, from *time.Time) {
	if !p.Known {
		return 0, 0, "", nil
	}
	taxRU = family.Banded([]family.TaxBand{{BP: rules.InvestmentBands[0].BP}}, p.InBase, rules.TaxUnitMinor)
	credited = min(max(p.WithheldInBase, 0), taxRU)
	if !haveBook {
		return taxRU, credited, "", nil
	}
	if _, err := countryCodeOf(country); err != nil {
		return taxRU, 0, TreatyNoCountry, nil
	}
	t, st := book.Of(country, p.On)
	switch st {
	case family.TreatyPartly:
		from = t.PartlySuspendedFrom
	case family.TreatySuspended:
		from = t.SuspendedFrom
	case family.TreatyEnded:
		from = t.EndedFrom
	}
	if !st.Credits() {
		credited = 0
	}
	return taxRU, credited, string(st), from
}

// ForeignSale is a disposal's line, named.
type ForeignSale struct {
	portfolio.Sale
	Paper string
	ISIN  string
}

// ForeignOf lays out year for the space's accounts at brokers abroad.
func (s *Service) ForeignOf(ctx context.Context, spaceID uuid.UUID, year int) (Foreign, error) {
	if s.rows == nil || s.papers == nil {
		return Foreign{}, fmt.Errorf("taxyear: foreign lines are not set up")
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return Foreign{}, err
	}
	rules := sp.CostBasisRules()
	if rules.TaxCurrency == "" {
		return Foreign{}, ErrNoEstimate
	}
	members, err := s.spaces.ListMembers(ctx, spaceID)
	if err != nil {
		return Foreign{}, err
	}
	var owner uuid.UUID
	for _, m := range members {
		if m.Role == family.RoleOwner {
			owner = m.ID
		}
	}
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return Foreign{}, err
	}
	out := Foreign{Year: year, Currency: rules.TaxCurrency, Accounts: []ForeignAccount{}}
	named := map[uuid.UUID]instrument.Instrument{}
	paper := func(id *uuid.UUID) (instrument.Instrument, error) {
		if id == nil {
			return instrument.Instrument{}, nil
		}
		if p, ok := named[*id]; ok {
			return p, nil
		}
		p, err := s.papers.ByID(ctx, *id)
		if err != nil {
			return instrument.Instrument{}, err
		}
		named[*id] = p
		return p, nil
	}
	book, haveBook, err := family.TreatiesOf(sp.TaxResidency)
	if err != nil {
		return Foreign{}, err
	}
	if haveBook {
		out.TreatiesAsOf = book.SourceAsOf
	}
	for _, a := range list {
		if a.Status != account.StatusActive || a.Type != account.TypeBrokerage || !a.ForeignBroker {
			continue
		}
		r, err := s.rows.YearRows(ctx, spaceID, a.ID, year, rules.TaxCurrency, rules.LongHoldingYears)
		if err != nil {
			return Foreign{}, fmt.Errorf("taxyear: account %s: %w", a.ID, err)
		}
		if len(r.Payouts) == 0 && len(r.Sales) == 0 && len(r.OtherTaxes) == 0 {
			continue
		}
		fa := ForeignAccount{ID: a.ID, Name: a.Name, Person: whose(a.Account, owner)}
		if i, _, ok := account.IntermediaryOf(a.Account); ok {
			fa.Country = i.Country
		}
		for _, p := range r.Payouts {
			line := ForeignPayout{Payout: p}
			switch p.Type {
			case portfolio.TypeDividend:
				line.Kind = KindDividends
			case portfolio.TypeCoupon:
				line.Kind = KindSecurities
			default:
				line.Kind = KindInterest
			}
			pp, err := paper(p.Instrument)
			if err != nil {
				return Foreign{}, err
			}
			line.Paper, line.ISIN = pp.Name, pp.ISIN
			if len(pp.ISIN) == 12 {
				line.Country = pp.ISIN[:2]
			}
			if !p.Known {
				fa.Unvalued++
			}
			line.TaxRU, line.Credit, line.Treaty, line.TreatyFrom = credit(p, line.Country, rules, book, haveBook)
			fa.Payouts = append(fa.Payouts, line)
		}
		for _, t := range r.OtherTaxes {
			pp, err := paper(t.Instrument)
			if err != nil {
				return Foreign{}, err
			}
			fa.Taxes = append(fa.Taxes, ForeignPayout{Payout: t, Paper: pp.Name, ISIN: pp.ISIN})
		}
		for _, sale := range r.Sales {
			pp, err := paper(&sale.Instrument)
			if err != nil {
				return Foreign{}, err
			}
			fa.Sales = append(fa.Sales, ForeignSale{Sale: sale, Paper: pp.Name, ISIN: pp.ISIN})
			if !sale.Known {
				fa.Unvalued++
				continue
			}
			fa.Income += sale.ProceedsInBase
			fa.Expenses += sale.FeeInBase
			for _, l := range sale.Lots {
				fa.Expenses += l.CostInBase
			}
		}
		out.Accounts = append(out.Accounts, fa)
	}
	slices.SortStableFunc(out.Accounts, func(a, b ForeignAccount) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
