package portfolio

import (
	"fmt"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// centsPerUnit shifts a major-unit decimal amount into minor units — the
// convention (2 decimal digits) used everywhere else in this codebase for
// amountMinor, e.g. marketdata.Converter (see its doc comment). Quote.Price
// and instrument-catalog prices are always expressed in major units of
// their currency.
const centsPerUnit = 2

// valuationGap names WHY a position has no market valuation, or why the one it
// has could not be brought into the position's own currency. It is what the
// contract's Position.market_value_gap publishes (see apiMarketValueGap), and
// it exists for the reason inBaseGap does: the answer travels from the code
// that decides it to the payload, instead of being reconstructed downstream
// from an absent figure.
//
// THE THREE CAUSES OF AN ABSENT VALUATION ARE NOT THE SAME NEWS, which is the
// whole of #78. Before this vocabulary the screen said «Нет котировки» over
// every empty valuation cell, and two of the three rows it said that over have
// a quote sitting right there: one whose type this program computes nothing
// for, and a bond whose face value nobody recorded. Both sent the reader off to
// wait for data that was not missing.
type valuationGap uint8

const (
	// valuationStruck: there is a valuation. It is also what marketValue
	// returns beside a refusal (see its err), where it means nothing: the
	// caller reads the error first and never publishes a gap on that path —
	// exactly as positionInBase returns inBaseStruck beside its own errors.
	valuationStruck valuationGap = iota
	// valuationTypeNotPriced: this program has no valuation model for the
	// instrument's type. Reported whether or not a quote exists, because a
	// quote closes nothing here.
	valuationTypeNotPriced
	// valuationNoFaceValue: a bond with no face value recorded. Its quote is a
	// percentage of face value, so there is nothing to take the percentage of,
	// and a quote closes nothing here either.
	valuationNoFaceValue
	// valuationNoQuote: the type is priced, the catalog row is complete, and no
	// price has been stored yet. The only one of the three an arriving quote
	// closes — which is why the two above are decided ahead of it.
	valuationNoQuote
	// valuationNoRateValuationCurrency: there IS a valuation, and the fx table
	// could not convert it out of the currency it is denominated in into the
	// position's own. Decided in toAPI rather than in marketValue, which knows
	// nothing about rates.
	valuationNoRateValuationCurrency
)

// apiMarketValueGap maps a gap onto the contract's vocabulary. ok is false for
// valuationStruck, which is not a gap at all and publishes no cause.
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

// marketValue computes a position's market value from the latest quote for
// its instrument, in minor units, plus the currency that value is
// denominated in. gap is valuationStruck when there is a figure, and otherwise
// names which of the three absences applies, in which case the caller must
// leave market_value_minor/market_value_currency/price/price_on null rather
// than publish a zero or misleading figure — and publish the gap, so that the
// dash on screen carries the reason it is there.
//
// quoted says whether q is a real quote rather than a zero value, and this
// function takes it rather than being called only when it is true. That is what
// puts the two permanent causes AHEAD of the missing quote: the switch below
// decides the type before it looks at the quote at all, and the bond branch
// checks its face value first. A crypto row with no quote therefore answers
// `type_not_priced`, not `no_quote` — because a quote arriving for it would
// still value nothing, and «no quote» would name the one thing whose arrival
// changes something. It is the same ordering rule apiInBaseGap states for
// in_base_gap: the cause no backfill closes is reported ahead of the ones it
// does.
//
// share/etf: price (per unit, major currency units) × quantity, in the
// quote's own currency (q.Currency). The QUOTE's, deliberately, because
// nothing makes it the instrument's: q.Currency is whatever the provider
// reported (MOEX sends one per row, in CURRENCYID), the instrument's comes
// from the catalog someone filled in, and the position's own — the currency
// this valuation is eventually compared against — comes from the journal,
// being the currency of the position's first operation (see Compute in
// engine.go). Three sources, nothing reconciling them.
//
// This comment used to say the price's currency and the instrument's "always
// agree here". They are only EXPECTED to, and the code around this function is
// already written for the case where they do not: toAPI converts a valuation
// that did not arrive in the position's currency, discloses the figure it
// converted from in market_value_source_currency/_minor, and publishes
// market_value_gap = no_rate_valuation_currency when there is no rate to
// convert with. A claim of "always" over a branch that handles "sometimes not"
// invites the next reader to delete the branch.
//
// bond: price is a percentage of face value (e.g. 95.20 meaning 95.20%), so
// the value is faceValueMinor × price/100 × quantity — already in minor
// units since faceValueMinor is. Crucially, that value is denominated in the
// face value's currency (faceCurrency, e.g. RUB for an OFZ), NOT the quote's
// currency: the quote's "currency" field for a bond is really just the unit
// the percentage price is quoted in, not a currency the resulting money is
// ever in. A bond with a face value but no face currency has no way to
// label that number, so it gets no valuation at all rather than a
// currency-less (and therefore meaningless) figure.
//
// Both branches multiply as decimals throughout and round exactly once at
// the end, half-away-from-zero (decimal.Decimal.Round's native behavior, the
// same rounding marketdata.Converter.Convert uses) — never float, never an
// intermediate round. That last step is money.Minor, which also refuses a
// product too large to be an int64 of minor units rather than letting it wrap
// (#27): a price and a quantity that each look ordinary can multiply past the
// edge, and the wrapped answer is a small figure of arbitrary sign.
//
// A QUANTITY AND A PRICE ARE NOW BOUNDED WHERE THEY ARE WRITTEN
// (operation.maxQuantity, #84) AND THIS GUARD STAYS REGARDLESS, because a figure
// that fitted when it was written can stop fitting afterwards. The price
// multiplied in here is the QUOTE's, which no validation of ours reaches; the
// bond branch below multiplies by a face value, which is the catalog's; a
// position is the sum of many operations and can pass the per-operation bound
// one accepted buy at a time; and the rows written before that bound existed are
// still in the journal, since no migration came with it.
//
// err is that refusal and NOTHING ELSE, which is why it is separate from gap.
// A gap says no valuation exists, and the caller answers it by publishing
// nulls plus the reason. An overflow answered that way would be a broken
// journal wearing the face of absent market data.
func marketValue(instType instrument.Type, faceValueMinor *int64, faceCurrency *string, quantity decimal.Decimal, q marketdata.Quote, quoted bool) (minor int64, currency string, gap valuationGap, err error) {
	switch instType {
	case instrument.TypeShare, instrument.TypeETF:
		if !quoted {
			return 0, "", valuationNoQuote, nil
		}
		minor, err = money.Minor(q.Price.Mul(quantity).Shift(centsPerUnit))
		if err != nil {
			return 0, "", valuationStruck, fmt.Errorf("%w: %s at %s", err, quantity, q.Price)
		}
		return minor, q.Currency, valuationStruck, nil
	case instrument.TypeBond:
		// Before the quote, deliberately: a bond's price is a percentage of
		// face value, so a quote arriving for a bond with no face value
		// recorded would still value nothing, and «no quote» would be an
		// answer the reader could act on where there is none.
		if faceValueMinor == nil || faceCurrency == nil {
			return 0, "", valuationNoFaceValue, nil
		}
		if !quoted {
			return 0, "", valuationNoQuote, nil
		}
		minor, err = money.Minor(decimal.NewFromInt(*faceValueMinor).Mul(q.Price).Shift(-centsPerUnit).Mul(quantity))
		if err != nil {
			return 0, "", valuationStruck, fmt.Errorf("%w: %s at %s%% of a face value of %d", err, quantity, q.Price, *faceValueMinor)
		}
		return minor, *faceCurrency, valuationStruck, nil
	default:
		// currency, crypto, metal, custom — and any type added to
		// instrument.Type without a branch above. This program computes no
		// value for them. The quote is not consulted, and that is the point:
		// such an instrument can carry a perfectly good quote (the seed's
		// sub-cent share is deliberately a SHARE for exactly this reason), and
		// the row still has no valuation. Nothing is coming — no decision to
		// write such a model has been taken — so this gap must never be
		// captioned as one that will close.
		return 0, "", valuationTypeNotPriced, nil
	}
}

// pricePerUnitMinor is what ONE bond costs in money at the quoted price, and is
// published beside the quote itself (Position.price_money_minor).
//
// IT IS FOR BONDS ALONE, because a bond is the only paper here whose quote is
// not money: it is a percentage of face value, and a reader comparing it with
// the cost and the valuation on the same row — both money — had to multiply by
// the face value in their head. For a share the quote already IS money per unit
// and this would restate it, so the caller does not ask.
//
// STRUCK ON ITS OWN, NOT DIVIDED OUT OF THE VALUATION. The two are the same
// product in a different order (face x price/100, times the quantity or not),
// and dividing would round a second time on a figure that had already been
// rounded once — the published value is the one that gets rounded, and this is
// a published value. It is deliberately NOT computed on the client either: the
// client does no money arithmetic, which is the rule that keeps every rounding
// decision in one place.
//
// The overflow refusal is marketValue's argument narrowed: this product has no
// quantity in it, so it fits wherever the valuation does — but the guard costs
// nothing and the alternative to it is a wrapped number that looks like a price.
func pricePerUnitMinor(faceValueMinor int64, price decimal.Decimal) (int64, error) {
	return money.Minor(decimal.NewFromInt(faceValueMinor).Mul(price).Shift(-centsPerUnit))
}
