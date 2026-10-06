package portfolio

import (
	"fmt"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// centsPerUnit shifts major units to minor units: two decimal places, the
// codebase-wide convention.
const centsPerUnit = 2

// valuationGap names why a position has no valuation, or why it could not be
// brought into the position's currency (Position.market_value_gap). The causes
// differ (#78): «Нет котировки» over a row that has a quote sends the reader to
// wait for nothing.
type valuationGap uint8

const (
	// valuationStruck: there is a valuation. Beside an error it means nothing.
	valuationStruck valuationGap = iota
	// valuationTypeNotPriced: no valuation model for the type; a quote closes
	// nothing.
	valuationTypeNotPriced
	// valuationNoFaceValue: a bond with no face value, so its percentage quote has
	// nothing to apply to.
	valuationNoFaceValue
	// valuationNoQuote: everything else is in place but no price is stored yet —
	// the only cause a quote closes.
	valuationNoQuote
	// valuationNoRateValuationCurrency: a valuation exists but could not be
	// converted into the position's currency; decided in toAPI.
	valuationNoRateValuationCurrency
)

// apiMarketValueGap maps a gap onto the contract; ok is false for
// valuationStruck.
func apiMarketValueGap(g valuationGap) (apitypes.MarketValueGap, bool) {
	switch g {
	case valuationTypeNotPriced:
		return apitypes.TypeNotPriced, true
	case valuationNoFaceValue:
		return apitypes.NoFaceValue, true
	case valuationNoQuote:
		return apitypes.NoQuote, true
	case valuationNoRateValuationCurrency:
		return apitypes.NoRateValuationCurrency, true
	default:
		return "", false
	}
}

// marketValue computes a position's market value in minor units and its
// currency, or names the gap (the caller then publishes nulls and the gap).
// quoted is passed in so the permanent causes (type, face value) are decided
// before the missing quote.
//
// share/etf: price × quantity, in the quote's currency — expected, not
// guaranteed, to be the position's; toAPI converts when not.
// bond: price is a percentage of face, so (face × price/100 + accrued interest)
// × quantity, in the face currency. The face and the interest are the
// exchange's for the day when the quote carries them (Quote.Bond), else the
// catalog's face and no interest. A face value without a currency gets no
// valuation.
//
// Decimal arithmetic, rounded once half away from zero by money.Minor, which
// refuses an overflow (#27). Write-time bounds (#84) do not make this guard
// redundant: quotes and face values come from elsewhere, positions sum many
// operations, and old rows predate the bounds. err is only that refusal.
func marketValue(instType instrument.Type, faceValueMinor *int64, faceCurrency *string, quantity decimal.Decimal, q marketdata.Quote, quoted bool) (minor int64, currency string, gap valuationGap, err error) {
	switch instType {
	// A coin is priced per unit like a share (decision Р-20).
	case instrument.TypeShare, instrument.TypeETF, instrument.TypeCrypto:
		if !quoted {
			return 0, "", valuationNoQuote, nil
		}
		minor, err = money.Minor(q.Price.Mul(quantity).Shift(centsPerUnit))
		if err != nil {
			return 0, "", valuationStruck, fmt.Errorf("%w: %s at %s", err, quantity, q.Price)
		}
		return minor, q.Currency, valuationStruck, nil
	case instrument.TypeBond:
		// The face value is checked before the quote: a quote would still value
		// nothing.
		face, currency, ok := bondFace(faceValueMinor, faceCurrency, q, quoted)
		if !ok {
			return 0, "", valuationNoFaceValue, nil
		}
		if !quoted {
			return 0, "", valuationNoQuote, nil
		}
		perUnit := face.Mul(q.Price).Shift(-2)
		if q.Bond != nil && q.Bond.Accrued != nil {
			perUnit = perUnit.Add(*q.Bond.Accrued)
		}
		minor, err = money.Minor(perUnit.Mul(quantity).Shift(centsPerUnit))
		if err != nil {
			return 0, "", valuationStruck, fmt.Errorf("%w: %s at %s%% of a face value of %s", err, quantity, q.Price, face)
		}
		return minor, currency, valuationStruck, nil
	default:
		// currency, metal, custom and any unhandled type: no valuation model, even
		// with a quote, and none is planned.
		return 0, "", valuationTypeNotPriced, nil
	}
}

// bondFace is the face a bond's quote is a percentage of, in major units, and
// its currency: the exchange's for the day when the quote carries it, else the
// catalog's. ok is false with neither.
func bondFace(faceValueMinor *int64, faceCurrency *string, q marketdata.Quote, quoted bool) (decimal.Decimal, string, bool) {
	if quoted && q.Bond != nil {
		return q.Bond.Face, q.Bond.Currency, true
	}
	if faceValueMinor == nil || faceCurrency == nil {
		return decimal.Zero, "", false
	}
	return decimal.NewFromInt(*faceValueMinor).Shift(-centsPerUnit), *faceCurrency, true
}

// pricePerUnitMinor is one bond's price in money (Position.price_money_minor),
// since a bond's quote is a percentage; accrued interest is not in it. Bonds
// only. Struck directly rather than divided out of the valuation, which would
// round twice; the client does no money arithmetic. Overflow-checked anyway.
func pricePerUnitMinor(face, price decimal.Decimal) (int64, error) {
	return money.Minor(face.Mul(price).Shift(-2).Shift(centsPerUnit))
}
