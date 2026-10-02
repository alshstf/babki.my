package portfolio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// baseGap names WHY a base-currency figure could not be struck. The two kinds
// are not the same news to the person reading the screen — one closes on its
// own and the other never will — and this type exists so the answer travels
// from the code that knows it to the payload, instead of being reconstructed
// downstream from flags that only correlate with it.
type baseGap uint8

const (
	// gapNone: nothing was missing; there is a figure.
	gapNone baseGap = iota
	// gapNoRate: every date the sum needs is known, but the fx table has no
	// rate for at least one of them yet. The backfill closes that gap on its
	// own and the figure appears later.
	gapNoRate
	// gapUndated: a parcel of basis does not know when it was bought (see
	// Lot.AcquiredOn), so there is no date to ask the fx table about and none
	// will ever be recovered — nobody wrote it down.
	gapUndated
)

// inBaseGap names WHICH TERM stopped a position's whole in_base object, and is
// what the contract's Position.in_base_gap publishes.
//
// It is baseGap's neighbour and deliberately not baseGap itself. That type
// answers for the account's realized total, which sums MANY positions, so the
// most it can name is the KIND of gap — no single term exists to point at, and
// it needs a `both` value because two kinds really can be true of one total at
// once. Here there is exactly one position, its terms are the very figures a
// row shows side by side, and the kind alone is not enough to caption them
// with: «нет курса» is false about a lot whose DATE nobody recorded, and
// «нет курса на дату покупки» is false over a market valuation whose row failed
// on today's rate. So this vocabulary names terms, and it needs no `both` — see
// apiInBaseGap for why one value is enough.
type inBaseGap uint8

const (
	// inBaseStruck: nothing was missing; there is an object.
	inBaseStruck inBaseGap = iota
	// inBaseSameCurrency: the position is already denominated in the base
	// currency, so there is no object and nothing to explain either — its own
	// figures ARE the base-currency ones. Named separately from inBaseStruck
	// purely for readability at the call site (see positionInBase's early
	// return): nothing downstream tells the two apart — apiInBaseGap maps both
	// to "no cause published" and handleList separates "struck" from "nothing
	// to convert" by checking the *PositionInBase pointer, never this value.
	inBaseSameCurrency
	// inBaseUndatedLot: a lot still held does not know when it was acquired
	// (see Lot.AcquiredOn and lotTerms), so there is no date to ask the fx
	// table about. The one member that never resolves on its own.
	inBaseUndatedLot
	// inBaseNoRateLotDate: every lot is dated, but the fx table has no rate for
	// at least one of those days, nor for any earlier one.
	inBaseNoRateLotDate
	// inBaseNoRateIncomeDate: the same, for the day one of the position's
	// income operations occurred.
	inBaseNoRateIncomeDate
	// inBaseNoRateToday: no rate today for the pair the market valuation needs
	// — the currency THAT VALUATION is in against the base one. Only a position
	// with a valuation to convert ever asks for it.
	inBaseNoRateToday
)

// apiInBaseGap maps a gap onto the contract's vocabulary. ok is false for the
// two members that are not gaps at all (inBaseStruck, inBaseSameCurrency),
// which publish no cause: one has a figure and the other has nothing to
// convert, and the client tells those apart by comparing the position's
// currency with the base one.
//
// EXACTLY ONE CAUSE IS EVER PUBLISHED, and that is a decision rather than a
// limitation of the type. positionInBase stops at the first term it cannot
// value, and it looks at them in the order the constants above are declared —
// which puts inBaseUndatedLot, the only member that no backfill will ever
// close, ahead of the three that it will. A row that has both a dateless lot
// and a missing rate therefore reports the dateless lot, and never promises a
// converted figure that is not coming. Among the three closeable ones, whichever
// is reported is true, and closing it reports the next: the caption converges
// instead of ever claiming more than the server knows. baseGap needs `both` for
// the opposite reason — it aggregates positions, so a permanent gap in one and a
// closeable gap in another are simultaneously true of the single total it
// describes.
func apiInBaseGap(g inBaseGap) (apitypes.InBaseGap, bool) {
	switch g {
	case inBaseUndatedLot:
		return apitypes.InBaseGapUndatedLot, true
	case inBaseNoRateLotDate:
		return apitypes.InBaseGapNoRateLotDate, true
	case inBaseNoRateIncomeDate:
		return apitypes.InBaseGapNoRateIncomeDate, true
	case inBaseNoRateToday:
		return apitypes.InBaseGapNoRateToday, true
	default:
		return "", false
	}
}

// realizedInBase is the position's realized result in currency to, or an
// explicit null with the kind of gap that stopped it. It answers for both
// readers of that figure — the position's own in_base block and the account's
// realized total — so the two can never disagree.
//
// ROUNDING HAPPENS ONCE, HERE, FOR THE WHOLE POSITION. The published quantity
// is one number per position, so that number is what gets rounded: every term
// of every disposal is multiplied as a decimal and only the total is rounded,
// half-away-from-zero, exactly as cost_minor and income_minor already are (see
// sumInBase). Rounding each disposal's own result first is the tempting shape —
// it reads like "convert each deal" — and it drifts from the true total by up to
// half a minor unit per disposal, in a figure the owner may well reconcile
// against a broker's report by hand. Nothing per-disposal is published, so
// nothing is made to agree by rounding earlier; the day a per-disposal
// breakdown IS published, each of those figures becomes a published quantity in
// its own right, is rounded once itself, and the contract will have to say that
// their sum can differ from this total by a unit — which is a statement about
// two roundings of the same money, not a defect in either. That per-disposal
// total, not this one, will be the legally correct figure to report on such a
// line-item breakdown: a tax authority computes the base per trade and sums
// the trades, not the reverse (НК РФ ст. 210 п. 5 taxes each disposal on its
// own), so the sum-of-rounded-rows answer is the one the law asks for even
// though this field will keep publishing the single round-once-per-position
// total for the reasons above.
//
// Null (rather than an error) covers the two ways a term can fail to be valued:
// no fx rate for a date the sum needs, and no purchase date for a parcel it
// retired. Both leave the sum one term short, and a total quietly missing a term
// is an invented number that looks exactly like a real one on screen. Neither
// touches the rest of the object: cost, income and the valuation are answers to
// their own questions, struck from their own dates, and none of them depends on
// a parcel that has already been sold.
//
// A non-nil error is a genuine failure (DB error, canceled context) that the
// caller must surface as a request error — never rendered as the null above,
// which would tell the owner their sale is unconvertible when the truth is that
// this server is having a bad minute.
func (h *Handler) realizedInBase(ctx context.Context, p *Position, to string, cache map[rateKey]*rateLookup) (nullable.Nullable[int64], baseGap, error) {
	if minor, inOneCurrency := p.RealizedPnL(); p.Currency == to && inOneCurrency {
		// Nothing to convert: the position's own realized result already IS
		// the base-currency one. The published `in_base` object is null for
		// such a position, but the figure is not unknown, and the account's
		// total would silently lose a real term if this answered otherwise.
		//
		// THE SHORTCUT NEEDS BOTH HALVES. A position whose currency is already
		// the base one can still have sold into another — a ruble account
		// holding a yuan bond redeemed for rubles is that row seen from the
		// other side — and there the position's own figure does not exist to be
		// handed over, while the base-currency one is perfectly strikeable from
		// the terms below. Testing only the currency published a null for a
		// figure this handler could compute.
		return nullable.NewNullableWithValue(minor), gapNone, nil
	}
	terms, dated := realizedTerms(p.Realizations, p.Currency)
	if !dated {
		return nullable.NewNullNullable[int64](), gapUndated, nil
	}
	minor, ok, err := h.sumInBase(ctx, terms, to, cache)
	if err != nil {
		return nullable.Nullable[int64]{}, gapNone, err
	}
	if !ok {
		return nullable.NewNullNullable[int64](), gapNoRate, nil
	}
	return nullable.NewNullableWithValue(minor), gapNone, nil
}

// incomeByInstrument groups the journal's instrument-attributed income
// operations by instrument, so each position's income can be converted payment
// by payment — at each payment's own rate, and out of each payment's own
// currency. Position.IncomeByCurrency answers neither question: it has kept the
// currencies but summed the dates away, and a total per currency could only
// ever be converted at one date of the many behind it.
//
// The type list must stay in lockstep with the engine's own notion of income
// (Compute: dividend, coupon and tax all book income, the tax through its
// negative amount; entries without an instrument are cash-level and never reach
// a position). Group by group and currency by currency, these operations
// therefore add up to exactly that position's IncomeByCurrency, and
// TestPositionInBaseIncomeUsesEachOperationsOwnRate exercises all three types,
// so a list that drifts from the engine's fails there instead of silently
// publishing a base income smaller than the income the same row reports in its
// own currency.
func incomeByInstrument(ops []Operation) map[uuid.UUID][]Operation {
	out := make(map[uuid.UUID][]Operation)
	for _, o := range ops {
		if o.InstrumentID == nil {
			continue
		}
		switch o.Type {
		case TypeDividend, TypeCoupon, TypeTax:
			out[*o.InstrumentID] = append(out[*o.InstrumentID], o)
		}
	}
	return out
}

// positionInBase expresses a position's cost, market value, unrealized P&L,
// income and realized P&L in baseCurrency. Every amount is valued at the fx
// rate that answers its own question, which is the whole point of this
// function:
//
//   - cost_minor sums the FIFO lots still held (p.Lots), each converted at the
//     rate of the day THAT lot was acquired. It is deliberately not
//     p.CostMinor times today's rate: that would price the basis as if the
//     whole position had been bought this morning — a question nobody asks —
//     and, by applying one rate to both sides of the subtraction below, would
//     cancel the currency's own move straight out of the profit, leaving
//     base-currency profit as nothing but position-currency profit times a
//     rate.
//   - income_minor sums the position's income operations, each at the rate of
//     the day it occurred AND out of the currency that payment arrived in,
//     which need not be the position's: a yuan bond pays its coupons in rubles
//     (see Position.IncomeByCurrency). Income, unlike the lots, is passed in —
//     see incomeByInstrument. This figure is therefore the position's WHOLE
//     income, unlike the position's own income_minor beside it, which can only
//     carry the part denominated in the position's currency (see toAPI).
//   - market_value_minor uses TODAY's rate, from the currency the valuation is
//     REALLY denominated in — a bond's face currency when the row had to
//     convert it (market_value_source_currency), the position's own otherwise.
//     It is the one current figure here, because "what is this holding worth"
//     is a question about now.
//   - unrealized_pnl_minor is that valuation minus that basis, both already in
//     baseCurrency, so it is exact integer subtraction with no second
//     rounding. Base-currency profit therefore INCLUDES the currency's
//     revaluation, and can differ from the position-currency profit in size
//     and even in sign — the owner's decision (2026-07-29): the two are honest
//     answers to two different questions, and the interface is what explains
//     which is which.
//   - realized_pnl_minor sums the disposals already made (p.Realizations), each
//     one's proceeds and fee at the rate of ITS day and each parcel of basis it
//     retired at the rate of the day THAT parcel was bought. It is the one
//     figure here that is settled: both of its ends are past events with dates
//     of their own, so unlike the unrealized figure above it will never move
//     again. It is passed IN rather than computed here (see realizedInBase),
//     because the account's own total needs exactly the same figure whether or
//     not this object survives — a settled result does not become unknowable
//     because today's rate is missing — and computing it twice would let the
//     two answers drift apart.
//
// fees_minor is deliberately excluded (owner feedback — not carried into
// PositionInBase at all, see the API contract).
//
// EVERY FIGURE IS CONVERTED FROM THE CURRENCY IT IS ACTUALLY IN, ONCE. cost is
// in the position's own currency and always was. Income is in whichever
// currency each payment arrived in — one sum here can hold rubles, dollars and
// yuan, each term converted out of its own (see incomeTerms). The market
// valuation may be in a third: a bond's is born in its face currency and toAPI brings
// it into the position's so the row can compare it with cost_minor (see
// toAPI). This object does NOT continue from that converted figure — it goes
// back to the original, market_value_source_minor in
// market_value_source_currency, and converts that straight into the base
// currency. One multiplication, one rounding. Chaining the two conversions
// rounded the money to a whole cent in a currency that appears nowhere in the
// answer, and then multiplied that lost fraction by the second rate: 1 000,00 €
// through the dollar came out 9 999 990 kopecks instead of 10 000 000, under a
// tooltip saying it had been converted from euros (#39). It had not been.
//
// market_value_minor is still published in_base ONLY when market_value_currency
// equals p.Currency, and unrealized_pnl_minor follows it. THAT RULE WITHHOLDS
// NOTHING THAT COULD BE COMPUTED, and the reason is checkable rather than a
// principle. Every fx row this program stores is quoted in RUB: the seed writes
// <currency>/RUB rows (cmd/babki/seed.go) and the only provider behind the
// refresh jobs is the CBR one, which publishes nothing else (marketdata/cbr).
// marketdata.resolveRate reaches a pair only as a direct row, as the inverse of
// one, or as a bridge X->RUB->Y. So for a valuation denominated in F on a
// position in P: this rule fires only after the F->P conversion in toAPI
// failed, which in a RUB-quoted table means F->RUB is missing or P->RUB is. A
// pair the table cannot resolve today it cannot resolve for any earlier day
// either (Store.FxRateOn takes the nearest EARLIER row), so a missing P->RUB
// has already taken cost_minor down with it and this function returned nil far
// above, before the rule was reached at all. What is left is a
// missing F->RUB — and then F->baseCurrency has no route either, whatever the
// base currency is. The figure is absent because there is none, not because one
// is being kept back.
//
// cost_minor is named alone there, and income_minor is not named beside it any
// more: a payment is converted out of the currency it arrived in, so a
// position's income need never touch P at all (see incomeTerms). It is the
// lots, whose costs are in P by construction, that carry the argument.
//
// The rule is nevertheless written out rather than left to that argument,
// because the argument is about the rate TABLE rather than about this function.
// The day one non-RUB-quoted row exists — a direct GBP/USD, say, which nothing
// in this codebase can currently create — a valuation stuck in GBP on a USD
// position becomes expressible in RUB, and withholding it turns into a real
// choice that would have to be argued for here instead of merely explained.
//
// The step "P->RUB missing has already nulled this object" holds only because
// no lot can be dated later than today: Service rejects an operation dated in
// the future (see its occurred_on check), and Store.FxRateOn resolves the
// nearest EARLIER row, so a table that answers any lot's date answers today's
// too. Contrapositive: no answer today means no answer on any lot date either,
// and cost_minor was unstrikable before this branch was reached.
//
// SOME ROWS STILL SLIP THROUGH, and the written-out rule covers them. A
// position holding no lot asks for no rate on P at all, and neither does its
// income unless a payment happened to arrive in P — so such a row survives a
// missing P->RUB, and for it F->RUB may well exist and the base-currency
// valuation be genuinely computable. What the rule withholds there is the zero
// of a closed row: with no lot there is no quantity (the lots' quantities sum to
// Position.Quantity), and a valuation is a price times nothing.
//
// Asymmetry between this object and the position's own figures is ordinary, not
// something this rule prevents: realized_pnl_minor goes null here while
// Position.realized_pnl_minor is published either way, and one undated lot nulls
// this whole object while every native figure stays. Null here is honest in all
// of those cases: the frontend falls back to showing the raw amount in its own
// currency with a "not converted" marker.
//
// WHEN p.Currency IS THE BASE CURRENCY THIS OBJECT IS NOT BUILT, and that is
// where widening income leaves a hole in the screen rather than filling one.
// Such a row has nothing to convert about its cost — its own figures ARE the
// base-currency ones — but it can still have received a payment in another
// currency, and this object is the only place a payment is ever converted. That
// payment then appears nowhere at all: not in the position's own income_minor,
// which carries the position's currency alone (see toAPI), and not here,
// because there is no here. What decides it is the shape of the contract: this
// object is specified as null on such a row, and income_minor is a single
// int64. Closing it means giving a position a per-currency income field, which
// is the next piece of work and not a decision this function may take alone.
//
// It returns no object — render in_base as null, the WHOLE object, never
// partially populated — together with the inBaseGap naming the term that
// stopped it, when p.Currency already equals baseCurrency (nothing
// to convert, and so no gap either: that one is inBaseSameCurrency, and the
// position's own figures ARE the base-currency ones), when any single rate the
// object needs is missing
// (marketdata.ErrNoRate): one lot's, one income operation's, or today's — and
// today's is needed only when there is a valuation for it to convert, so a
// position with no usable quote publishes its basis and its income without
// ever asking for a rate for today (rate_on is then null, saying exactly
// that). Today's is now asked for the VALUATION's currency against the base
// one, not the position's, so on a bond priced off a foreign face value a
// missing rate there takes cost_minor and income_minor down with it as well —
// unreachable in a RUB-quoted table, by the same kind of argument as the rule
// above and spelled out at the today's-rate block below, and stated here
// because it is a fact about the whole object rather than about the valuation
// alone. It also returns
// no object when a single lot does not know WHEN it was acquired
// (Lot.AcquiredOn nil),
// which leaves no date to ask for a rate in the first place. A basis summed
// from only the lots that happened to convert is an invented number, smaller
// than the truth and indistinguishable from a real one on screen; it would
// drag the P&L along with it. The two causes are one rule — a term that cannot
// be valued voids the whole sum — and differ only in whether the date is
// missing or the rate for it is. This differs from
// market_value_minor/unrealized_pnl_minor inside the returned object, which
// are null when there is no usable quote or the valuation isn't in
// p.Currency, and from realized_pnl_minor, which is null when a disposal's own
// day has no rate or a parcel it retired has no purchase date — those nulls are
// confined to the figures that could not be struck.
//
// What decides between the two is whether the unvaluable term belongs to the
// object as a whole or to one figure in it. A lot still held sits inside
// cost_minor's sum, and cost_minor is what unrealized_pnl_minor is measured
// against, so its failure travels; a parcel sold last year sits inside nothing
// but the realized sum, and taking the rest of the position down with it would
// hide a basis and a valuation that are perfectly well known.
//
// A non-nil error means a genuine failure (DB error, canceled context) that
// the caller must surface as a request error — never silently rendered as
// null, which would misrepresent an outage as "nothing to convert". The gap
// returned beside such an error is inBaseStruck and means nothing: an outage is
// not one of the gaps this vocabulary describes, and handleList reads the error
// first (mirroring realizedInBase, which returns gapNone on the same path).
//
// EVERY RETURN NAMES BOTH AT ONCE, which is the point of the second result
// rather than a flag computed beside the call: the sentence a reader is shown
// and the figure they are not shown leave this function in one statement, so
// the caption cannot come to describe a different failure than the one that
// actually happened. handleList closes the loop from the other end — it
// publishes the object only when the gap says there was none (see there).
// now is the request's one reading of "today" (see toAPI, which takes it for
// the same reason): the valuation this object converts was itself brought into
// p.Currency at today's rate a moment earlier, and the two must mean the same
// day even for a request that crosses UTC midnight.
func (h *Handler) positionInBase(ctx context.Context, p *Position, apiPos apitypes.Position, income []Operation, baseCurrency string, realizedMinor nullable.Nullable[int64], now time.Time, cache map[rateKey]*rateLookup) (*apitypes.PositionInBase, inBaseGap, error) {
	// NOTHING TO CONVERT — ALMOST ALWAYS. A position denominated in the base
	// currency publishes every figure it has already in that currency, and a
	// second object repeating them under the same sign would say nothing.
	//
	// THE EXCEPTION IS A DISPOSAL THAT SETTLED IN A THIRD CURRENCY. Then the
	// position's own realized figure does not exist at all
	// (Position.RealizedPnL), while a base-currency one is perfectly strikeable
	// from the disposal's terms — and this object is the only place the payload
	// has to put it. Returning early here published a position whose realized
	// result appeared NOWHERE: a null in its own currency, and no object beside
	// it to carry the answer.
	//
	// The rest of the object is then a set of identity conversions, and that is
	// the point rather than a cost: every figure in it equals the position's
	// own, so nothing about it can contradict the row it stands on.
	if _, inOneCurrency := p.RealizedPnL(); p.Currency == baseCurrency && inOneCurrency {
		return nil, inBaseSameCurrency, nil
	}

	lots, dated := lotTerms(p.Lots, p.Currency)
	if !dated {
		// One lot does not know when it was acquired, so its basis cannot be
		// valued at all (see lotTerms). The whole object goes, exactly as it
		// does when one lot's date has no fx rate: a basis summed from only the
		// lots that could be converted is smaller than the truth, looks like an
		// ordinary figure on screen, and drags the profit down with it. Nothing
		// is published rather than something wrong, and the position still
		// shows every figure it has in its own currency.
		//
		// This is checked before any rate is asked for, which is also what
		// settles the answer for a position that has this gap AND a missing
		// rate: the permanent cause is the one reported (see apiInBaseGap).
		return nil, inBaseUndatedLot, nil
	}
	costMinor, ok, err := h.sumInBase(ctx, lots, baseCurrency, cache)
	if err != nil {
		return nil, inBaseStruck, err
	}
	if !ok {
		return nil, inBaseNoRateLotDate, nil
	}

	// Each payment out of the currency IT arrived in (see incomeTerms), which
	// is why this sum takes no source currency: a position's income can hold
	// three currencies at once and the position's own is not necessarily one of
	// them.
	incomeMinor, ok, err := h.sumInBase(ctx, incomeTerms(income), baseCurrency, cache)
	if err != nil {
		return nil, inBaseStruck, err
	}
	if !ok {
		return nil, inBaseNoRateIncomeDate, nil
	}

	// Unlike the two sums above, a realized result that cannot be struck nulls
	// only itself: its terms are disposals already made, and nothing else in
	// this object is computed from them (see realizedInBase).
	out := &apitypes.PositionInBase{
		CostMinor:        costMinor,
		IncomeMinor:      incomeMinor,
		RealizedPnlMinor: realizedMinor,
		Currency:         baseCurrency,
	}
	// Both terms are already converted here — each at the rate of its own date —
	// and the income one covers every payment whatever currency it arrived in.
	// So this figure exists on rows where the position-currency one cannot,
	// which is the whole reason it is worth publishing twice.
	if !realizedMinor.IsNull() && realizedMinor.IsSpecified() {
		settled, err := money.Add(realizedMinor.MustGet(), incomeMinor)
		if err != nil {
			return nil, inBaseStruck, fmt.Errorf("%w: settled result of instrument %s in %s, a realized %d and income of %d",
				err, p.InstrumentID, baseCurrency, realizedMinor.MustGet(), incomeMinor)
		}
		out.SettledMinor = nullable.NewNullableWithValue(settled)
	} else {
		out.SettledMinor = nullable.NewNullNullable[int64]()
	}
	out.TotalMinor = nullable.NewNullNullable[int64]()

	// Which figure this object converts, and out of which currency. The GUARD
	// comes first and the rule it enforces is unchanged: a valuation that never
	// reached the position's currency is not carried into the base one (see this
	// function's doc comment — in a table where every rate is quoted in RUB,
	// there is no base-currency figure to carry).
	//
	// WHAT IT ASKS changed with the gap. It used to compare
	// market_value_currency with p.Currency, which is toAPI's outcome read back
	// out of the payload; it now asks toAPI's own answer, market_value_gap. The
	// two are equivalent today — the failed conversion is the only way the two
	// currencies can differ, and the `c == nil` half of the old condition was
	// already redundant, since toAPI sets market_value_minor and
	// market_value_currency together and a nil valuation is handled below
	// regardless. What the change buys is that the cause published to the reader
	// and the figure withheld from them are one decision instead of two that
	// must keep agreeing: a
	// caption saying the valuation could not be converted, over a converted
	// valuation, is precisely the failure this whole change is about.
	//
	// ANY gap withholds it, which is the same rule stated once for four values
	// rather than one guard per value (#78 gave the field three more). Three of
	// them say no valuation was struck at all, and on those rows this line
	// changes nothing — market_value_minor is already nil. The fourth is the one
	// it was written for. That the guard is now partly redundant is what makes
	// it right: "a gap of any kind means there is no valuation this object may
	// carry" holds for whatever value is added next, where a guard naming
	// `no_rate_valuation_currency` would silently stop covering it.
	marketValueMinor := nullableValue(apiPos.MarketValueMinor)
	valuationCurrency := p.Currency
	if nullableValue(apiPos.MarketValueGap) != nil {
		marketValueMinor = nil
	}
	// Only then is the converted figure swapped back for the original it was
	// converted FROM, so the base-currency answer is struck in one step.
	//
	// `marketValueMinor != nil` here is UNREACHABLE INSURANCE, not part of the
	// rule: toAPI sets the two source fields only in the branch where the
	// conversion succeeded, which is not the branch that sets the gap, so the
	// guard above never struck the figure this would put back. Removing the
	// conjunct leaves the whole suite green, and it is kept anyway — not as a
	// second, quieter statement of the rule (the guard states it), but so that a
	// future toAPI which set the source fields on a path that did NOT convert
	// could not resurrect, here, a valuation the guard has just withheld. What no
	// test can pin, a comment has to say.
	if src, srcCurrency := nullableValue(apiPos.MarketValueSourceMinor), nullableValue(apiPos.MarketValueSourceCurrency); marketValueMinor != nil && src != nil && srcCurrency != nil {
		marketValueMinor, valuationCurrency = src, *srcCurrency
	}
	if marketValueMinor == nil {
		// No valuation this object may convert — no usable quote, or one the
		// guard above withheld because it never reached the position's own
		// currency. Nothing is being kept back in that second case: the
		// conversion out of its own currency needs the very rate whose absence
		// stopped it from reaching p.Currency (see the doc comment).
		// rate_on goes with it, because rate_on is the DATE OF that figure and
		// of nothing else here: the basis and the income are struck at the
		// rates of their own many dates, and a date published beside them
		// would be naming the day of a value this object does not contain (the
		// one place in this payload where the label claimed slightly more than
		// the object held — see PositionInBase.rate_on in the contract).
		//
		// Today's rate is not asked for at all in this branch, and that is the
		// point rather than an optimization: it is required by exactly the
		// figures it values, so a position with no quote publishes the cost and
		// the income it does know instead of going null over a rate that would
		// have appeared nowhere in the answer. Before, the object was refused
		// whenever today's rate was missing, and stayed correct only by
		// coincidence — Store.FxRateOn resolves the nearest EARLIER date, so a
		// table holding any lot's rate holds one for today too, and the two
		// conditions happened to fire together on real data. Coincidence is not
		// a rule, and TestPositionInBasePublishedWithoutTodaysRateWhenThereIsNoQuote
		// pins the rule.
		out.MarketValueMinor = nullable.NewNullNullable[int64]()
		out.UnrealizedPnlMinor = nullable.NewNullNullable[int64]()
		out.RateOn = nullable.NewNullNullable[string]()
		// The object stands, so nothing stopped IT — which is why this returns
		// no gap even though a figure inside it is null. The one case that has
		// something to explain is already published on the position itself, by
		// the code that decided it (Position.market_value_gap, set in toAPI);
		// the other, a position with no usable quote, has no valuation to
		// withhold and nothing to caption.
		return out, inBaseStruck, nil
	}

	// Today's rate values the market valuation and supplies rate_on — the one
	// date in this object that is both unambiguous and worth disclosing, since
	// it is how fresh the "what is it worth now" figure is, and per
	// Store.FxRateOn it can be older than today whenever the rate table is
	// stale. Without it the valuation cannot be struck, and since the profit
	// below is measured against a basis that would then be published beside a
	// valuation that is not, the whole object goes rather than half of it.
	//
	// The pair is the VALUATION's currency against the base one, which for a
	// bond priced off a foreign face value is neither the position's currency
	// nor the one any other figure in this object is converted from. That is
	// the whole point (see the doc comment): one screen can therefore ask
	// about EUR->RUB for a row whose every other figure is USD->RUB, and
	// rateQueries enumerates it from the same marketValue call that decides it.
	//
	// It also widens what the ErrNoRate below can take down. Before, only the
	// POSITION currency's today-rate could null this object; now a missing rate
	// for a bond's FACE currency nulls cost_minor and income_minor too, though
	// neither is converted through it. Nothing reaches that today: this line
	// runs only when the valuation did arrive in p.Currency, so toAPI resolved
	// F->p.Currency, so — every rate this program stores being quoted in RUB —
	// the table holds F->RUB; and cost_minor was struck through
	// p.Currency->baseCurrency at each lot's own date, so RUB->baseCurrency sits
	// there on a day no later than today, which by Store.FxRateOn's
	// nearest-earlier rule answers for today as well; F->baseCurrency is the two
	// of them bridged. (A position holding no lot strikes its basis without any
	// rate at all and escapes the second half of that — it has nothing but zeros
	// to lose here.) It is written down because what changed is the SHAPE of the
	// failure rather than anything reachable: the day a non-RUB-quoted row
	// exists, this refusal becomes reachable, and it refuses figures that have
	// nothing to do with the valuation.
	today := h.rateFor(ctx, valuationCurrency, baseCurrency, now, cache)
	if today.err != nil {
		if errors.Is(today.err, marketdata.ErrNoRate) {
			return nil, inBaseNoRateToday, nil
		}
		return nil, inBaseStruck, today.err
	}
	valuation, err := today.applyTo(*marketValueMinor)
	if err != nil {
		// Too large to state in the base currency. The missing rate handled
		// just above nulls the whole object and names a gap, because the figure
		// it stops is one the fx backfill will supply later; this one is not
		// waiting for anything, so it fails the request rather than joining
		// that null. The gap it returns beside the error is inBaseStruck — not
		// a cause, because nothing here is a gap the caller should publish: a
		// non-nil error means the request dies and no gap is ever read.
		return nil, inBaseStruck, err
	}
	out.MarketValueMinor = nullable.NewNullableWithValue(valuation)
	// Guarded for the same reason the position's own unrealized figure is (see
	// toAPI): two int64s of opposite sign can differ by more than an int64, and
	// a negative valuation needs nothing worse than a negative face value to
	// arrive. Both operands have been converted already, so this is the last
	// arithmetic between here and the wire.
	unrealized, err := money.Sub(valuation, costMinor)
	if err != nil {
		// Named, because money.Sub names no figure and a 500 whose log says only
		// "does not fit" tells whoever reads it nothing about which row to look at.
		return nil, inBaseStruck, fmt.Errorf("%w: a base valuation of %d less a basis of %d in %s",
			err, valuation, costMinor, baseCurrency)
	}
	out.UnrealizedPnlMinor = nullable.NewNullableWithValue(unrealized)
	total, err := totalToAPI(out.SettledMinor, out.UnrealizedPnlMinor, p.InstrumentID)
	if err != nil {
		return nil, inBaseStruck, err
	}
	out.TotalMinor = total
	// rate_on names the rate that was actually applied, and there is not always
	// one to name: a valuation already denominated in the base currency (an OFZ
	// with a ruble face value in a dollar account of a ruble space) is the
	// answer as it stands, and rateFor hands back a rate of 1 on the ZERO date
	// — marketdata resolves nothing for from == to, deliberately, so that an
	// identity conversion cannot disclose a staleness that does not exist. That
	// zero date formats as "0001-01-01", so the choice here is between an
	// explicit null and a caption naming a rate that had no part in the figure.
	// Null, then — for the same reason the branch above publishes one.
	if today.date.IsZero() {
		out.RateOn = nullable.NewNullNullable[string]()
	} else {
		out.RateOn = nullable.NewNullableWithValue(today.date.Format("2006-01-02"))
	}
	return out, inBaseStruck, nil
}
