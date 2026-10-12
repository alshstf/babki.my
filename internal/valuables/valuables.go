// Package valuables keeps the family's bars and coins of precious metal and
// the places they are kept (decisions Р-39 А, Р-40 Г): each piece on its own,
// with its maker, number, certificate and day of purchase — what decides when
// it sells free of tax and whether a bank takes it back without an assay —
// valued twice, by the central bank's price and as it would sell now.
// Tables: valuables_storages, valuables_items, valuables_bank_prices.
package valuables

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// Place is where a storage keeps its pieces.
type Place string

const (
	PlaceHome    Place = "home"
	PlaceSafeBox Place = "safe_box"
	PlaceCustody Place = "custody"
	PlaceOther   Place = "other"
)

// FeePeriod is what a storage's fee is paid for.
type FeePeriod string

const (
	FeeMonth FeePeriod = "month"
	FeeYear  FeePeriod = "year"
)

// Storage is a place's terms: its bank, until when, for how much a period,
// and what below the central bank's price the bank buys the metal back for
// — nil for the program's guide (Guide).
type Storage struct {
	Place     Place
	Bank      string
	Until     *time.Time
	FeeMinor  *int64
	FeePeriod *FeePeriod
	Buyback   *decimal.Decimal
}

// Kind is what a piece is.
type Kind string

const (
	KindBar  Kind = "bar"
	KindCoin Kind = "coin"
)

// Item is a bar or a coin. Weight is in grams; Fineness in thousandths
// (999.9 is 99.99 % metal).
type Item struct {
	ID          uuid.UUID
	AccountID   uuid.UUID
	Kind        Kind
	Metal       marketdata.Metal
	Weight      decimal.Decimal
	Fineness    decimal.Decimal
	Maker       string
	ForeignMade bool
	Serial      string
	Certificate bool
	Sealed      bool
	BoughtOn    time.Time
	CostMinor   int64
	Currency    string
	OwnerUserID *uuid.UUID
	Note        string
}

// Pure is the grams of metal in the piece.
func (it Item) Pure() decimal.Decimal {
	return it.Weight.Mul(it.Fineness).Div(decimal.NewFromInt(1000))
}

// taxFreeYears: a sale of metal held three years and more is free of tax
// (НК РФ ст. 217.1; ФНС, Приморский край, 26.02.2026 — «облагается, если
// владели меньше трёх лет»).
const taxFreeYears = 3

// TaxFreeFrom is the day from which the piece sells free of tax.
func (it Item) TaxFreeFrom() time.Time { return it.BoughtOn.AddDate(taxFreeYears, 0, 0) }

// Hard is a reason a bank may not take a piece back at its price without an
// assay.
type Hard string

const (
	HardForeign       Hard = "foreign_made"
	HardNoCertificate Hard = "no_certificate"
	HardOpened        Hard = "opened"
)

// HardToSell are the piece's reasons, in that order; none for a sealed
// Russian piece with its certificate.
func (it Item) HardToSell() []Hard {
	out := []Hard{}
	if it.ForeignMade {
		out = append(out, HardForeign)
	}
	if !it.Certificate {
		out = append(out, HardNoCertificate)
	}
	if !it.Sealed {
		out = append(out, HardOpened)
	}
	return out
}

// Guide is the program's guess of what below the central bank's price a bank
// buys a piece back for, in percent, by metal and weight (decision Р-40 Г).
// Read off surveys, not banks' own prices — to be checked against them
// (alshstf/babki.my#510); a storage's own percent stands before it.
func Guide(metal marketdata.Metal, weight decimal.Decimal) decimal.Decimal {
	switch metal {
	case marketdata.Gold:
		switch {
		case weight.GreaterThanOrEqual(decimal.NewFromInt(100)):
			return decimal.NewFromInt(5)
		case weight.GreaterThanOrEqual(decimal.NewFromInt(20)):
			return decimal.NewFromInt(7)
		case weight.GreaterThanOrEqual(decimal.NewFromInt(5)):
			return decimal.NewFromInt(10)
		}
		return decimal.NewFromInt(15)
	case marketdata.Silver:
		// Silver is bought with 20 % VAT, which a buyback does not return.
		return decimal.NewFromInt(25)
	}
	return decimal.NewFromInt(15)
}

// BankPrice is what the family's bank buys a gram of the metal back for, as
// written in on a day.
type BankPrice struct {
	Metal marketdata.Metal
	Price decimal.Decimal
	On    time.Time
}

// bankPriceDays is how long a bank's price written in stands (Р-40 Г).
const bankPriceDays = 30

// SellBy says where a piece's «if sold now» comes from.
type SellBy string

const (
	SellByBankPrice  SellBy = "bank_price"
	SellByStorage    SellBy = "storage_percent"
	SellByGuide      SellBy = "guide_percent"
	SellByNoPriceYet SellBy = "no_price"
)

// Value is a piece valued: by the central bank's price (Market) and as it
// would sell now (Sell), in roubles' minor units; nil without the bank's
// price. Percent is what Sell is below Market when not by the bank's price.
type Value struct {
	Market, Sell *int64
	By           SellBy
	Percent      *decimal.Decimal
	BankOn       *time.Time
	PriceOn      *time.Time
}

// Valuing values a piece: the central bank's price per gram (nil when there
// is none yet) and its day, the storage's terms, the bank's prices written
// in, and today.
func Valuing(it Item, cbr *marketdata.MetalPrice, st Storage, bank []BankPrice, today time.Time) Value {
	pure := it.Pure()
	for _, b := range bank {
		if b.Metal == it.Metal && !b.On.Before(today.AddDate(0, 0, -bankPriceDays)) {
			sell := pure.Mul(b.Price).Mul(decimal.NewFromInt(100)).Round(0).IntPart()
			on := b.On
			v := Value{Sell: &sell, By: SellByBankPrice, BankOn: &on}
			if cbr != nil {
				market := pure.Mul(cbr.Price).Mul(decimal.NewFromInt(100)).Round(0).IntPart()
				day := cbr.On
				v.Market, v.PriceOn = &market, &day
			}
			return v
		}
	}
	if cbr == nil {
		return Value{By: SellByNoPriceYet}
	}
	marketDec := pure.Mul(cbr.Price).Mul(decimal.NewFromInt(100))
	market := marketDec.Round(0).IntPart()
	percent, by := Guide(it.Metal, it.Weight), SellByGuide
	if st.Buyback != nil {
		percent, by = *st.Buyback, SellByStorage
	}
	sell := marketDec.Mul(decimal.NewFromInt(100).Sub(percent)).Div(decimal.NewFromInt(100)).Round(0).IntPart()
	day := cbr.On
	return Value{Market: &market, Sell: &sell, By: by, Percent: &percent, PriceOn: &day}
}
