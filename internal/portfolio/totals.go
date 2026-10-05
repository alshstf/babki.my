package portfolio

import (
	"fmt"
	"sort"

	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// accountTotals adds up what the account HAS MADE — every position's total
// (realized result + income + unrealized revaluation) plus the account's own
// charges, which no position can see.
//
// It is realizedTotals' bigger sibling and works the same way: two forms because
// the screen has two modes, the server adds and the client renders, and the
// terms added are the ROUNDED per-position figures published in the same
// response rather than the raw terms re-summed (see AccountTotal.in_base in the
// contract — a header that disagreed with the rows one field away would be a
// number nobody could check).
//
// TWO ASSUMPTIONS RIDE ALONG WITH IT, and both are published as counts rather
// than buried:
//
//   - A HOLDING NOTHING PRICES GOES IN AT NOUGHT. Its basis counts as spent and
//     nothing counts as held, so the total is lower than the truth by whatever
//     the paper is really worth. This is the owner's decision, taken over the
//     alternative of publishing no total at all while a single frozen fund sits
//     in the account, and it is the conservative reading of a paper nobody can
//     sell. zeroValued counts them and zeroValuedCost says how much basis went
//     in that way, so the size of the assumption is a number.
//   - A HOLDING THAT DOES NOT KNOW WHAT IT COST goes in with its whole market
//     value as profit, which pushes the total the other way. Nothing can be done
//     about it here — the broker never sent the price — so it is counted and
//     named too.
//
// THE ACCOUNT'S OWN CHARGES are the terms no row carries: interest credited on
// the cash, commissions booked as operations of their own, and the tax taken
// from the account rather than from a payment. Commissions charged ON a trade
// are deliberately NOT among them: a purchase capitalizes its commission into
// the lot's cost and a disposal subtracts its own from the proceeds, so both are
// already inside the position figures being added here, and taking them again
// would charge the owner twice for one commission.
type accountTotals struct {
	baseCurrency string
	byCurrency   map[string]int64
	// unknowable names the currencies whose bucket is missing a term, for the
	// same reason realizedTotals.notInOneCurrency does: a position that
	// realized into another currency, or was paid in one, has no own-currency
	// total to add, and a bucket short a term is a number that reads as a
	// result rather than as a gap.
	unknowable     map[string]bool
	inBaseMinor    int64
	noRate         bool
	zeroValued     int
	zeroValuedCost map[string]int64
	unknownCost    int
	// undatedPositions counts the holdings left out of the base figure because
	// their purchase dates were never recorded (see addPosition).
	undatedPositions int
	// noRateCurrencies names the money this account holds that could not be
	// valued. It is filled from the CASH alone, where the missing rate is
	// exactly that currency against the base one — a position's gap can be
	// about the currency its valuation is struck in rather than its own, and
	// naming that one would be a guess (see AccountTotal.no_rate_currencies).
	noRateCurrencies map[string]bool
	// cashFXMinor is the part of inBaseMinor that is the currency result on the
	// account's money — held and spent (decision Р-5: shown as its own line).
	// cashFXSeen says some money in another currency contributed; cashFXGap
	// that a rate behind it is missing, so the part has no figure.
	cashFXMinor int64
	cashFXSeen  bool
	cashFXGap   bool
}

func newAccountTotals(baseCurrency string) *accountTotals {
	return &accountTotals{
		baseCurrency:     baseCurrency,
		byCurrency:       make(map[string]int64),
		unknowable:       make(map[string]bool),
		zeroValuedCost:   make(map[string]int64),
		noRateCurrencies: make(map[string]bool),
	}
}

// addPosition folds one row in, in both currencies at once.
//
// The four cases it separates are the whole of the rule. A row with a total
// contributes it. A row with no valuation AT ALL contributes its settled result
// less its basis — the paper counted at nought — and is counted as such. A row
// whose total is missing for any other reason (nothing settled in this currency,
// or a valuation struck in a currency this row cannot be compared with) makes
// the bucket unknowable, because the term genuinely does not exist rather than
// being nought. And the base figure answers all three again from its own object,
// which carries its own arithmetic (see PositionInBase).
func (at *accountTotals) addPosition(p apitypes.Position, inBase *apitypes.PositionInBase, gap inBaseGap, realizedGap baseGap) error {
	// Some of the paper — held or already sold — arrived with no price and
	// counts as bought for nothing, so its whole value is in this total as
	// profit. Counted before anything else, because it is true of the row
	// whatever else is or is not known about it.
	if p.HasUnknownCost {
		at.unknownCost++
	}

	native, nativeOK := rowTotal(p.TotalMinor, p.SettledMinor, p.CostMinor, p.MarketValueGap.IsSpecified() && !p.MarketValueGap.IsNull())
	if !nativeOK {
		at.unknowable[p.Currency] = true
	} else {
		sum, err := money.Add(at.byCurrency[p.Currency], native.minor)
		if err != nil {
			return fmt.Errorf("%w: the account's total in %s, adding %d to %d",
				err, p.Currency, native.minor, at.byCurrency[p.Currency])
		}
		at.byCurrency[p.Currency] = sum
		if native.atZero {
			// COUNTED FROM THE ROW'S OWN CURRENCY, deliberately, even though the
			// mark stands beside the base figure. The base branch below writes
			// off the same paper for the same reason — nothing prices it — so
			// counting there instead would say the same thing, and counting in
			// both would say it twice.
			//
			// The one row the two branches disagree about is a paper with no
			// quote whose payments arrived in a third currency: it has no
			// own-currency total to write off (the bucket is unknowable) while
			// its base figure is struck perfectly well. Such a row goes into the
			// base total at nought and is NOT counted here — the figure it
			// qualifies is right and the count beside it is one short. Left as
			// it is rather than papered over, because the alternative shapes
			// each say something false: counting on the base branch would count
			// nothing at all on an account with no conversion objects, and
			// counting on both would double every ordinary row.
			at.zeroValued++
			cost, err := money.Add(at.zeroValuedCost[p.Currency], p.CostMinor)
			if err != nil {
				return fmt.Errorf("%w: the basis counted at nought in %s, adding %d to %d",
					err, p.Currency, p.CostMinor, at.zeroValuedCost[p.Currency])
			}
			at.zeroValuedCost[p.Currency] = cost
		}
	}

	// The row's own gap, mapped onto the two words this total has. The dateless
	// lot is the one that never closes, so it lands on `undated`; the three rate
	// gaps land on `no_rate`, which says a figure may yet appear. Both of the
	// non-gaps fall through: one has an object, the other has nothing to
	// convert because the row is already in the base currency.
	switch gap {
	case inBaseUndatedLot:
		// LEFT OUT, NOT SUPPRESSING. Nobody knows when this paper was bought, so
		// its basis has no day whose rate could value it — and unlike a missing
		// rate, that never resolves: a date cannot arrive later. Taking the
		// whole account's figure down for ever over it answered nothing, and on
		// the owner's own journal it left five accounts of six blank.
		//
		// The count is what keeps this honest, and it is the same bargain the
		// owner struck for a paper nothing prices: publish the figure, and say
		// what it rests on.
		at.undatedPositions++
		return nil
	case inBaseNoRateLotDate, inBaseNoRateIncomeDate, inBaseNoRateToday:
		at.noRate = true
		return nil
	}
	// No gap covers two shapes: an object was struck, or the position is
	// already in the base currency and there was nothing to convert — in which
	// case its own figures ARE the base ones. Reading the pointer is what tells
	// them apart, exactly as handleList does when it publishes them.
	base, baseOK := native, nativeOK
	if inBase != nil {
		base, baseOK = rowTotal(inBase.TotalMinor, inBase.SettledMinor, inBase.CostMinor,
			p.MarketValueGap.IsSpecified() && !p.MarketValueGap.IsNull())
	}
	if !baseOK {
		// The base figure has no settled result to build on, and WHY decides
		// what to do about it. A disposal whose parcels have no acquisition day
		// can never be valued — the same permanent gap the branch above answers,
		// so the paper is left out and counted. A missing RATE is the opposite:
		// the figure appears when the backfill catches up, and publishing a
		// total without this paper meanwhile would quietly change it later.
		if realizedGap == gapUndated {
			at.undatedPositions++
			return nil
		}
		at.noRate = true
		return nil
	}
	sum, err := money.Add(at.inBaseMinor, base.minor)
	if err != nil {
		return fmt.Errorf("%w: the account's total in %s, adding %d to %d",
			err, at.baseCurrency, base.minor, at.inBaseMinor)
	}
	at.inBaseMinor = sum
	return nil
}

// rowTotalValue is one row's contribution and whether the paper behind it was
// counted at nought.
type rowTotalValue struct {
	minor  int64
	atZero bool
}

// rowTotal reads one row's contribution out of the three figures the contract
// publishes for it. Shared by both currencies because the shapes are identical:
// PositionInBase carries a total, a settled result and a basis under the same
// names and the same nullability rules as the row itself.
//
// unvalued is the row's own answer to "is there a valuation at all" — the gap,
// which is non-null on exactly that row and null both when a valuation was
// struck and when the row is a closed position with nothing left to value.
func rowTotal(total, settled nullable.Nullable[int64], costMinor int64, unvalued bool) (rowTotalValue, bool) {
	if !total.IsNull() {
		return rowTotalValue{minor: total.MustGet()}, true
	}
	if unvalued && !settled.IsNull() {
		// Counted at nought: the settled result stands, and the basis of what
		// is still held is written off. money.Sub rather than a bare minus —
		// both terms survived money.Minor, their difference need not (#83).
		minor, err := money.Sub(settled.MustGet(), costMinor)
		if err != nil {
			return rowTotalValue{}, false
		}
		return rowTotalValue{minor: minor, atZero: costMinor != 0}, true
	}
	return rowTotalValue{}, false
}

// addCharge folds one of the account's own charges in: interest, a commission
// booked as its own operation, or tax taken from the account. baseMinor is the
// same amount converted at the rate of the day it was charged, null when no rate
// for that day exists.
func (at *accountTotals) addCharge(currency string, minor int64, baseMinor nullable.Nullable[int64]) error {
	sum, err := money.Add(at.byCurrency[currency], minor)
	if err != nil {
		return fmt.Errorf("%w: the account's total in %s, adding a charge of %d to %d",
			err, currency, minor, at.byCurrency[currency])
	}
	at.byCurrency[currency] = sum
	if baseMinor.IsNull() {
		at.noRate = true
		return nil
	}
	base, err := money.Add(at.inBaseMinor, baseMinor.MustGet())
	if err != nil {
		return fmt.Errorf("%w: the account's total in %s, adding a charge of %d to %d",
			err, at.baseCurrency, baseMinor.MustGet(), at.inBaseMinor)
	}
	at.inBaseMinor = base
	return nil
}

// addCash folds one currency's money in — the currency's own result, and the
// only term of this total that has nothing to do with any paper.
//
// BASE CURRENCY ONLY, and that is not an omission. In its own currency a
// thousand yuan cost a thousand yuan and is worth a thousand yuan: there is no
// result to add, today or ever, and adding a nought to each bucket would be
// noise. In the base currency the same money has both halves of one — what it
// earned on the way out (realized) and what it is worth against what it cost
// (unrealized) — and those are exactly the two figures a reader means by "what
// did my currency do".
//
// WHY BOTH HALVES AND NOT ONE. Take a hundred thousand rubles to dollars at 100
// and back at 120: the money made twenty thousand rubles and the balances
// afterwards value to nought, because everything it earned is in the departure.
// Take dollars and hold them: nothing has left, and everything is in the
// unrealized half. An account does both.
//
// NOTHING HERE IS DOUBLE-COUNTED WITH THE PAPERS. A share bought with dollars
// spends dollar parcels — banking the currency's move up to that day — and the
// share's own basis is struck at the rate of the day it was bought, so the two
// figures meet at that day and neither covers the other's ground.
func (at *accountTotals) addCash(c apitypes.CashPosition) error {
	if c.Currency == at.baseCurrency {
		// Rubles in a ruble space: both halves are structurally nought, and
		// asking for them would be asking a rate of one to say something.
		return nil
	}
	// An overdraft contributes what it EARNED and nothing else. Its unrealized
	// half does not exist (see cashToAPI), and that absence is not a gap in the
	// data: there is no gain on money the account does not have, so nothing is
	// withheld over it and no currency is named.
	halves := []nullable.Nullable[int64]{c.InBase.RealizedPnlMinor, c.InBase.UnrealizedPnlMinor}
	if !c.InBase.Gap.IsNull() && c.InBase.Gap.MustGet() == apitypes.CashGapNegativeBalance {
		halves = []nullable.Nullable[int64]{c.InBase.RealizedPnlMinor}
	}
	at.cashFXSeen = true
	for _, half := range halves {
		if half.IsNull() {
			at.cashFXGap = true
			// A rate behind this money is missing, so the account has no single
			// figure — and the currency is named, because «нет курса» alone
			// cannot tell a gap that closes when the backfill catches up from
			// one that never closes at all. The Bank of Russia publishes no rate
			// for XAU, the code the broker uses for gold, and no amount of
			// waiting will produce one.
			at.noRate = true
			at.noRateCurrencies[c.Currency] = true
			continue
		}
		sum, err := money.Add(at.inBaseMinor, half.MustGet())
		if err != nil {
			return fmt.Errorf("%w: the account's total in %s, adding %d of currency result to %d",
				err, at.baseCurrency, half.MustGet(), at.inBaseMinor)
		}
		at.inBaseMinor = sum
		fx, err := money.Add(at.cashFXMinor, half.MustGet())
		if err != nil {
			return fmt.Errorf("%w: the account's currency result in %s, adding %d to %d",
				err, at.baseCurrency, half.MustGet(), at.cashFXMinor)
		}
		at.cashFXMinor = fx
	}
	return nil
}

// result is the account's answer as the contract publishes it.
func (at *accountTotals) result() apitypes.AccountTotal {
	out := apitypes.AccountTotal{
		BaseCurrency:             at.baseCurrency,
		ByCurrency:               make([]apitypes.AccountCurrencyTotal, 0, len(at.byCurrency)),
		ZeroValuedPositions:      at.zeroValued,
		ZeroValuedCostByCurrency: make([]apitypes.CurrencyAmount, 0, len(at.zeroValuedCost)),
		NoRateCurrencies:         make([]string, 0, len(at.noRateCurrencies)),
		UndatedPositions:         at.undatedPositions,
		UnknownCostPositions:     at.unknownCost,
	}
	// Every currency that has a bucket OR a hole in one: a currency whose only
	// news is that it cannot be totalled must still say so, or the account
	// silently has one fewer currency than it holds.
	seen := make(map[string]bool, len(at.byCurrency)+len(at.unknowable))
	for currency := range at.byCurrency {
		seen[currency] = true
	}
	for currency := range at.unknowable {
		seen[currency] = true
	}
	for currency := range seen {
		entry := apitypes.AccountCurrencyTotal{
			Currency:    currency,
			AmountMinor: nullable.NewNullableWithValue(at.byCurrency[currency]),
		}
		if at.unknowable[currency] {
			entry.AmountMinor = nullable.NewNullNullable[int64]()
		}
		out.ByCurrency = append(out.ByCurrency, entry)
	}
	sort.Slice(out.ByCurrency, func(i, j int) bool {
		return out.ByCurrency[i].Currency < out.ByCurrency[j].Currency
	})
	for currency, minor := range at.zeroValuedCost {
		out.ZeroValuedCostByCurrency = append(out.ZeroValuedCostByCurrency,
			apitypes.CurrencyAmount{Currency: currency, AmountMinor: minor})
	}
	sort.Slice(out.ZeroValuedCostByCurrency, func(i, j int) bool {
		return out.ZeroValuedCostByCurrency[i].Currency < out.ZeroValuedCostByCurrency[j].Currency
	})

	for currency := range at.noRateCurrencies {
		out.NoRateCurrencies = append(out.NoRateCurrencies, currency)
	}
	sort.Strings(out.NoRateCurrencies)

	if at.noRate {
		out.InBase = nullable.NewNullNullable[int64]()
		out.InBaseGap = nullable.NewNullableWithValue(apitypes.NoRate)
	} else {
		out.InBase = nullable.NewNullableWithValue(at.inBaseMinor)
		out.InBaseGap = nullable.NewNullNullable[apitypes.RealizedGap]()
	}
	out.CashFxInBase = nullable.NewNullNullable[int64]()
	if at.cashFXSeen && !at.cashFXGap {
		out.CashFxInBase = nullable.NewNullableWithValue(at.cashFXMinor)
	}
	return out
}

// accountCharges are the journal entries the account is charged or credited
// DIRECTLY, which no position figure contains: interest on the cash, every
// commission booked as an operation of its own, and the tax taken from the
// account rather than withheld from a payment.
//
// EACH EXCLUSION IS A CLAIM ABOUT THE ENGINE, and each is checked there rather
// than assumed here. A commission charged on a trade is capitalized into the
// lot's cost (buy) or subtracted from the proceeds (sell), so it is already
// inside the totals being added — while an operation of type fee touches
// nothing but Position.FeesByCurrency, which no published total reads, and would
// vanish entirely if it were not taken here. A tax attributed to an instrument
// is already inside that position's income; one without an instrument reaches no
// position at all. Interest is refused by type and never reaches a position
// either.
//
// Amount and fee are taken together — an entry's cash effect is its amount less
// its own commission, the same formula the reconciliation uses — so a charge
// that carries one does not lose it.
func accountCharges(ops []Operation) []Operation {
	var out []Operation
	for _, o := range ops {
		switch o.Type {
		case TypeInterest, TypeFee:
			out = append(out, o)
		case TypeTax:
			if o.InstrumentID == nil {
				out = append(out, o)
			}
		}
	}
	return out
}

// realizedTotals adds up what an account's closed deals have locked in, folding
// each position in as it is built.
//
// THE SERVER ADDS, THE CLIENT RENDERS. Nothing here converts and nothing here
// rounds — every term was converted and rounded once already, for its own
// position — so a client could in principle do this addition itself and get the
// same integers. It does not, for the same reason the accounts screen's totals
// are computed here and not there: a figure the interface shows is derived in
// one place, so the policy behind it (what rounds, which positions count, what
// a gap suppresses) can change in one place and every reader keeps agreeing on
// the answer. The single standing exception to that rule is the profit
// percentage, and it is an exception precisely because a percentage is not
// money.
//
// Two forms, because the screen has two modes and the server cannot know which
// one is on: by currency (each position in its own, never added across
// currencies) and in the space's base currency (one figure, or none at all).
type realizedTotals struct {
	baseCurrency string
	byCurrency   map[string]int64
	// notInOneCurrency names the currencies whose bucket is missing a term
	// because some position in it realized into another currency and has no
	// own-currency figure at all (see Position.RealizedPnL). The bucket is
	// published as a null rather than as the sum of the rest: a total quietly
	// short one of its terms is an invented number that looks exactly like a
	// real one, which is the rule the base total below already stands on.
	notInOneCurrency map[string]bool
	inBaseMinor      int64
	// undatedPositions counts the positions LEFT OUT of the base sum because a
	// disposal released a parcel nobody knows the purchase day of. That gap is
	// permanent — no rate answers for a day that was never recorded — so the
	// position is excluded and counted rather than taking the account's whole
	// figure down for good. The same bargain accountTotals strikes, and the
	// owner's ruling for both (#158, #195).
	undatedPositions int
	// unknownCostPositions counts the positions whose disposals sold shares
	// that arrived with no purchase price: their whole proceeds are in both
	// totals as profit (see UnknownCost).
	unknownCostPositions int
	noRate               bool
}

func newRealizedTotals(baseCurrency string) *realizedTotals {
	return &realizedTotals{
		baseCurrency:     baseCurrency,
		byCurrency:       make(map[string]int64),
		notInOneCurrency: make(map[string]bool),
	}
}

// add folds one position in: nativeMinor is its realized result in its own
// currency, which the contract publishes unconditionally, and baseMinor the
// same result in the base currency — null with the gap that stopped it.
//
// The base sum keeps accumulating even after a gap is seen: the terms cost
// nothing to add, and result() drops the partial sum rather than publishing it.
//
// BOTH ACCUMULATIONS ARE GUARDED, and int64 addition is what they are guarded
// against. Every term arriving here is an ordinary figure — it had to survive
// money.Minor to be an int64 at all, and realizedInBase rounded it once for its
// own position — and a total of ordinary terms can still leave the range, at
// which point Go's + wraps as silently as decimal.IntPart() does. The same
// argument sumInBase and marketdata.ConvertMany already stand on: the guard
// belongs on the figure that gets published, and here that figure is the sum
// (#83). Reachable through the base total in particular, where no fx rate is
// bounded from above.
//
// A REFUSAL IS AN ERROR AND NOT A GAP. `undated` and `no_rate` say a term could
// not be valued and the figure may yet appear; this total's terms are all
// present and one of them is broken, so it is handed to the caller, which fails
// the request (see handleList). Nothing is half-added on the way out: both
// totals are computed before either is stored, so a refused position leaves the
// accumulator exactly as it found it.
func (rt *realizedTotals) add(currency string, nativeMinor nullable.Nullable[int64], baseMinor nullable.Nullable[int64], gap baseGap, soldUnknown bool) error {
	native := rt.byCurrency[currency]
	if nativeMinor.IsNull() {
		// The position has no figure in its own currency, so this bucket has
		// none either — and it is marked BEFORE the base total is touched
		// below, because the two answers are independent: the base figure is
		// struck from the disposals' own terms and survives perfectly well
		// (see realizedInBase).
		rt.notInOneCurrency[currency] = true
	} else {
		var err error
		if native, err = money.Add(rt.byCurrency[currency], nativeMinor.MustGet()); err != nil {
			return fmt.Errorf("%w: the account's realized total in %s, adding %d to %d",
				err, currency, nativeMinor.MustGet(), rt.byCurrency[currency])
		}
	}
	inBase, undated := rt.inBaseMinor, 0
	switch gap {
	case gapUndated:
		undated++
	case gapNoRate:
		rt.noRate = true
	default:
		// gapNone: the figure was struck, so there is one to add.
		var err error
		if inBase, err = money.Add(rt.inBaseMinor, baseMinor.MustGet()); err != nil {
			return fmt.Errorf("%w: the account's realized total in %s, adding %d to %d",
				err, rt.baseCurrency, baseMinor.MustGet(), rt.inBaseMinor)
		}
	}
	rt.byCurrency[currency], rt.inBaseMinor = native, inBase
	rt.undatedPositions += undated
	if soldUnknown {
		rt.unknownCostPositions++
	}
	return nil
}

// result is the account's answer as the contract publishes it.
//
// Rounding: the per-position figures added here were each rounded once, for
// their own position, and this total is their exact integer sum. Re-deriving it
// from the raw disposal terms and rounding once for the whole account would be
// the more accurate single number — and would put the header a minor unit away
// from the very rows it stands over, in the same response. See
// RealizedTotal.in_base in the API contract.
func (rt *realizedTotals) result() apitypes.RealizedTotal {
	out := apitypes.RealizedTotal{
		BaseCurrency: rt.baseCurrency,
		ByCurrency:   make([]apitypes.RealizedCurrencyTotal, 0, len(rt.byCurrency)),
	}
	for currency, minor := range rt.byCurrency {
		total := apitypes.RealizedCurrencyTotal{
			Currency:         currency,
			RealizedPnlMinor: nullable.NewNullableWithValue(minor),
		}
		if rt.notInOneCurrency[currency] {
			total.RealizedPnlMinor = nullable.NewNullNullable[int64]()
		}
		out.ByCurrency = append(out.ByCurrency, total)
	}
	sort.Slice(out.ByCurrency, func(i, j int) bool {
		return out.ByCurrency[i].Currency < out.ByCurrency[j].Currency
	})

	// A missing RATE withholds the figure: the rate arrives, and a total
	// published without that position meanwhile would quietly change later. A
	// missing purchase DAY never arrives, so those positions are left out and
	// counted beside the figure instead (see undatedPositions).
	out.UndatedPositions = rt.undatedPositions
	out.UnknownCostPositions = rt.unknownCostPositions
	if rt.noRate {
		out.InBase = nullable.NewNullNullable[int64]()
		out.InBaseGap = nullable.NewNullableWithValue(apitypes.NoRate)
	} else {
		out.InBase = nullable.NewNullableWithValue(rt.inBaseMinor)
		out.InBaseGap = nullable.NewNullNullable[apitypes.RealizedGap]()
	}
	return out
}
