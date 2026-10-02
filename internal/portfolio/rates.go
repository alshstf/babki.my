package portfolio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

// rateKey identifies one memoized fx rate lookup: the pair being converted AND
// the date its rate must come from. The date belongs in the key because this
// handler no longer converts everything at today's rate — each lot is valued at
// the rate of the day it was acquired and each income operation at the rate of
// the day it occurred (see positionInBase) — so a cache keyed by currency alone
// would serve the first date's rate for every later date, producing wrong
// numbers that look entirely plausible on screen. Copied from
// operation.rateKey, which keys the journal's per-operation conversions the
// same way and for the same reason.
//
// The date is held as its YYYY-MM-DD string rather than a time.Time so the
// key compares the calendar date itself, immune to two otherwise-equal
// time.Time values differing in monotonic clock reading or *time.Location
// pointer (which would merely cost extra lookups, but would do so invisibly).
//
// The TARGET currency is part of the key because this screen has two of them.
// Almost everything here converts into the space's base currency, but a bond
// whose valuation is denominated in its face currency is brought into the
// POSITION's currency instead — that is the whole point of that conversion,
// comparability with cost_minor (see toAPI) — so one request can legitimately
// ask for the SAME source currency under two different targets: a bond with a
// USD face value held in a EUR position needs USD->EUR today, while a plain
// USD position on the same screen needs USD->RUB today — both "USD, today"
// (see TestPositionsSharedRateMemoKeepsTargetsApart, which is exactly this
// pair of positions). A key naming only the source would collide the two,
// filing one position's rate where the other looks for its own: the same
// silently-plausible wrong number the date is in the key to prevent.
// operation.rateKey has one target and says so; this one cannot.
type rateKey struct {
	from string
	to   string
	on   string
}

// newRateKey is the only place a lookup becomes a key. Both halves of the memo
// build one — the loop asking for a rate (rateFor) and the prefetch filing the
// answers before it (prewarmRates) — and a key spelled two ways would file
// every prefetched answer where nothing looks for it: no wrong number, just a
// batch paid for and then ignored, which is precisely the kind of failure that
// leaves no trace.
func newRateKey(from, to string, on time.Time) rateKey {
	return rateKey{from: from, to: to, on: on.Format("2006-01-02")}
}

// rateLookup memoizes one rateKey's resolved fx rate — the rate itself, the
// date it actually came from, and the resolution error — so a request hits the
// fx rate store at most once per distinct (pair, date). A position with many
// lots usually has few distinct purchase dates, and positions sharing a
// currency share every lookup; "today" is one entry for the whole request.
// Mirrors account's and operation's identically named type.
type rateLookup struct {
	rate decimal.Decimal
	date time.Time
	err  error
}

// applyTo converts amountMinor at this rate — multiply as decimals, round the
// product once, half-away-from-zero — which is exactly what
// marketdata.Converter.Convert does with the rate it resolves.
//
// Both figures this file strikes from a single rate go through it: a bond's
// valuation brought into the position's own currency (toAPI, which used to
// call Convert and now shares the memo like everything else) and that
// valuation carried on into the base currency (positionInBase). One statement
// of how a rate becomes money, so the two cannot round differently from each
// other. It is deliberately the same step marketdata.Converter.Convert applies
// — but that is two statements of one rule, not one, and only tests hold them
// together (Convert has no production caller left in this package). If
// Convert's scale handling ever changes, for zero-decimal currencies or
// anything else, this has to be changed with it. Amounts summed from many
// rates do not come through
// here — they must round once for the whole sum, not once per term (see
// sumInBase).
//
// A product that does not fit in an int64 of minor units is refused rather
// than wrapped (money.ErrOverflow, #27). Callers surface that as a request
// error: it is a figure this server cannot state, not a figure it has yet to
// learn, and every null on this screen means the latter.
func (rl *rateLookup) applyTo(amountMinor int64) (int64, error) {
	minor, err := money.Minor(decimal.NewFromInt(amountMinor).Mul(rl.rate))
	if err != nil {
		return 0, fmt.Errorf("%w: %d at a rate of %s", err, amountMinor, rl.rate)
	}
	return minor, nil
}

// datedMinor is one amount, the currency it is denominated in, and the date
// whose fx rate values it: a lot's remaining cost with the day that lot was
// acquired, an income payment's amount with the day it occurred.
//
// THE CURRENCY TRAVELS WITH THE TERM rather than being one argument for the
// whole sum, because one sum's terms need not share it. A position's income can
// arrive in several currencies at once — a dollar share paying a ruble dividend
// with a ruble tax withheld (see portfolio.Position.IncomeByCurrency) — and
// converting those payments out of the position's currency would apply a dollar
// rate to an amount of rubles. The basis and the disposals are in the position's
// own currency by construction, and their terms say so one by one like every
// other.
type datedMinor struct {
	minor int64
	from  string
	on    time.Time
}

// rateFor resolves from->to on date on, memoized in cache for the rest of the
// request. The returned rateLookup carries the resolution error rather than
// returning it, because callers must tell marketdata.ErrNoRate (an expected
// outcome that nulls in_base) apart from a genuine failure (which fails the
// request) — see positionInBase.
//
// The cache is normally already full when this is called: handleList prefetches
// every rate the screen was expected to want in one round trip (see
// prewarmRates). A MISS IS NOT AN ERROR — it is the whole safety net. Whatever
// the enumeration failed to predict, or the batch failed to fetch, is resolved
// here one pair at a time exactly as it was before any of that existed, so the
// figures on the screen never depend on the prefetch being complete or even on
// its having succeeded. Only their cost does.
func (h *Handler) rateFor(ctx context.Context, from, to string, on time.Time, cache map[rateKey]*rateLookup) *rateLookup {
	key := newRateKey(from, to, on)
	rl, ok := cache[key]
	if !ok {
		rate, date, err := h.conv.Rate(ctx, from, to, on)
		rl = &rateLookup{rate: rate, date: date, err: err}
		cache[key] = rl
	}
	return rl
}

// sumInBase converts every amount at the fx rate of its OWN date, out of its
// OWN currency, and returns the total in currency to. Every amount is
// multiplied as a decimal and only
// the total is rounded, once, half-away-from-zero — the same final step
// marketdata.Converter.Convert applies to a single amount. Rounding each term
// instead could drift from the true total by a minor unit per term, and the
// total is the figure actually published.
//
// ok is false when at least one date has no rate at all
// (marketdata.ErrNoRate): the caller must then publish nothing rather than a
// total quietly missing one of its terms. err is reserved for genuine
// failures (DB error, canceled context, or a total too large to be an int64 of
// minor units), which must fail the request instead.
//
// The overflow guard sits on the TOTAL, because the total is the published
// figure: every term can be an ordinary amount and their sum still leave the
// range. It is an error and not ok=false for the reason that distinction
// exists at all — ok=false is answered with a null the screen reads as data
// that has yet to arrive, and a sum too large to state is not waiting for
// anything.
func (h *Handler) sumInBase(ctx context.Context, amounts []datedMinor, to string, cache map[rateKey]*rateLookup) (minor int64, ok bool, err error) {
	total := decimal.Zero
	for _, a := range amounts {
		rl := h.rateFor(ctx, a.from, to, a.on, cache)
		if rl.err != nil {
			if errors.Is(rl.err, marketdata.ErrNoRate) {
				return 0, false, nil
			}
			return 0, false, rl.err
		}
		total = total.Add(decimal.NewFromInt(a.minor).Mul(rl.rate))
	}
	minor, err = money.Minor(total)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %d terms totalling %s %s", err, len(amounts), total, to)
	}
	return minor, true, nil
}

// lotTerms flattens the lots a position still holds into the terms of ONE sum
// — its basis — each term carrying the date whose fx rate values it: the day
// THAT lot was acquired, never today's (see positionInBase for why).
//
// dated is false as soon as one lot does not know when it was acquired (see
// portfolio.Lot.AcquiredOn): it arrived by a transfer whose purchase dates
// were never recorded. Its basis is real money, but there is no date to value
// it at, and every candidate date — the transfer's, another lot's, today's —
// would be a number this handler made up. What the caller does about that is
// the caller's decision (positionInBase publishes nothing at all); this
// function only refuses to invent the date.
//
// It is realizedTerms' twin for the held side, and like realizedTerms it is
// called from two places: by the code that converts these terms and by the
// enumeration that prefetches their rates (see rateQueries). That is the point
// of it being a function at all — one statement of which dates the basis
// needs, so the prefetch cannot come to disagree with the sum it is
// prefetching for.
// It asks anyUndatedLot first and walks the lots a second time to build the
// terms, rather than bailing out mid-loop as it used to. The second walk costs
// a nil check per lot and no allocation; what it buys is that the answer
// published as `undated_lot` and the answer published as has_undated_lots are
// one predicate rather than two that must agree (see anyUndatedLot). The
// dereference below is safe for exactly that reason, and a broken predicate
// would panic here rather than quietly value a lot at a date it does not have.
func lotTerms(lots []Lot, currency string) (terms []datedMinor, dated bool) {
	if anyUndatedLot(lots) {
		return nil, false
	}
	terms = make([]datedMinor, 0, len(lots))
	for _, l := range lots {
		if l.AcquiredOn == nil {
			// Bought for nothing as far as anyone knows (see DatelessBasis):
			// nought on any day, so no term and no rate.
			continue
		}
		// The position's currency, passed in: a lot's cost is what was paid for
		// the paper, and Position.Currency is exactly the currency that was paid
		// (every operation that adds a lot settles it — see
		// Type.mustMatchPositionCurrency).
		terms = append(terms, datedMinor{minor: l.CostMinor, from: currency, on: *l.AcquiredOn})
	}
	return terms, true
}

// incomeTerms flattens a position's income operations into the terms of ONE
// sum, each at the rate of the day it occurred and OUT OF THE CURRENCY IT
// ARRIVED IN (see incomeByInstrument for which operations those are, and
// positionInBase for why not one rate for the total).
//
// The currency is read off each operation rather than taken from the position,
// and that is the difference between this and its two sibling functions. A
// position's payments need not share the currency its cost is in — a yuan bond
// pays its coupons in rubles, a dollar share's dividend and the tax withheld on
// it arrive in rubles (see portfolio.Position.IncomeByCurrency) — so one sum
// here can hold terms in three currencies, each converted out of its own. Using
// the position's currency for all of them would multiply an amount of rubles by
// a dollar rate and publish the product.
//
// There is no undated case: an operation always has the day it occurred on.
// Like lotTerms and realizedTerms, it is called both by the sum and by the
// enumeration that prefetches the sum's rates.
func incomeTerms(income []Operation) []datedMinor {
	terms := make([]datedMinor, 0, len(income))
	for _, o := range income {
		terms = append(terms, datedMinor{minor: o.AmountMinor, from: o.Currency, on: o.OccurredOn})
	}
	return terms
}

// realizedTerms flattens every disposal a position has made into the terms of
// ONE sum — the position's realized result — each term carrying the date whose
// fx rate values it.
//
// A disposal contributes three kinds of term, and the dates are the whole
// point: the proceeds and the fee happened on the day of the disposal, while
// each parcel of basis it retired was paid for on the day THAT parcel was
// bought (НК РФ ст. 210 п. 5). Converting the event's own net result at one
// rate — even the correct rate for its own day — would price the expense on a
// day it was not incurred and quietly cancel the currency's move between
// purchase and sale out of the answer, which is a real part of the result and
// not a rounding of it.
//
// The engine records amortizations as disposals alongside sales, and transfers
// as none (see Realization), so which events reach this function is settled
// there rather than re-decided here.
//
// dated is false whenever any retired parcel does not know when it was bought
// (see Lot.AcquiredOn): there is no date to ask the fx table about, and every
// candidate — the disposal's own day, another parcel's, today's — would be a
// number this handler made up. The caller publishes nothing for the realized
// figure then; it does NOT drop the terms it could value, since a result
// missing part of its expense reads as a larger profit, not as a gap.
//
// It is lotTerms' twin for the realized side, and like lotTerms it asks
// anyUndatedRealization first and walks the events a second time to build the
// terms, rather than bailing out mid-loop as it used to (see anyUndatedLot):
// the answer published as has_undated_realizations and the answer published
// as RealizedTotal's `undated` gap are one predicate rather than two that must
// agree. The dereference below is safe for exactly that reason.
func realizedTerms(events []Realization, currency string) (terms []datedMinor, dated bool) {
	if anyUndatedRealization(events) {
		return nil, false
	}
	terms = make([]datedMinor, 0, len(events)*2)
	for _, e := range events {
		// TWO CURRENCIES, AND EACH TERM IS TAGGED WITH ITS OWN. The proceeds and
		// the fee are in the currency the disposal settled in
		// (Realization.Currency) — usually the position's and not always, since
		// a yuan bond may be redeemed for rubles. The retired basis is in the
		// position's, always: what the purchases behind it were denominated in
		// is exactly what the currency rule settles
		// (Operation.mustMatchPositionCurrency).
		//
		// This is what makes the base-currency figure exist for a position whose
		// own-currency figure does not: nothing here ever subtracts one currency
		// from another — every term is converted first, at its own date, and only
		// then summed.
		terms = append(terms,
			datedMinor{minor: e.ProceedsMinor, from: e.Currency, on: e.OccurredOn},
			datedMinor{minor: -e.FeeMinor, from: e.Currency, on: e.OccurredOn},
		)
		for _, r := range e.Released {
			if r.AcquiredOn == nil {
				// Sold as bought for nothing (see DatelessBasis): no expense
				// to convert, and the whole proceeds above are the result.
				continue
			}
			terms = append(terms, datedMinor{minor: -r.CostMinor, from: currency, on: *r.AcquiredOn})
		}
	}
	return terms, true
}

// rateQueries enumerates every fx rate the loop in handleList is about to ask
// for, so one RatesOn call can resolve them all and every rateFor below finds
// its answer already in the memo. A screen holding thirty positions bought on
// a hundred days between them costs one round trip for the lot, instead of one
// per distinct date per position (#40, #53).
//
// IT IS DERIVED FROM THE CODE THAT CONSUMES THE RATES WHEREVER THAT IS
// POSSIBLE, RATHER THAN WRITTEN BESIDE IT. Every historical date here comes
// out of lotTerms, incomeTerms or realizedTerms — the same three functions the
// sums themselves are built from — and the one pair whose target is not the
// base currency comes out of marketValue, the same function toAPI values the
// position with. That leaves no list of DATES to keep in step with a second
// list of dates, which is where this codebase has been bitten before: two
// computations of one value drift apart, and a prefetch is the worst place for
// it, since the two disagree in silence.
//
// THE SOURCE CURRENCIES COME OFF THE SAME TERMS, and now they must: a term
// carries the currency it is denominated in (see datedMinor), so a position's
// income — which need not be in the position's currency at all — enumerates one
// pair per payment, in whatever currency that payment arrived, without this
// function having to know that income is the sum which behaves so.
//
// ONE thing is not derived, and it is a DATE rather than a pair: the two
// valuation queries below are written `now` by hand. toAPI and positionInBase
// both value a holding at today's rate as a flat decision rather than by
// building terms, so there is nothing here to call for it and these two lines
// restate that choice. Their PAIRS are derived like everything else — both come
// out of the one marketValue call in the loop, so neither restates which
// currency a bond's valuation is really in. If the valuation ever stops being
// struck at today's rate, these two dates have to follow — and nothing will
// make them, because the consequence is a missed prefetch, not a wrong figure
// (see below).
//
// Completeness is an optimization, not a correctness condition, and the
// asymmetry is deliberate. Asking for a rate the loop turns out not to need
// (the valuation below whose conversion fails, say) costs one row in a query
// that was happening anyway. FAILING to ask for one costs nothing but a round
// trip either, because rateFor resolves whatever it does not find, exactly as
// it did before this existed. What would be dangerous is naming a DIFFERENT
// pair than the loop asks for and having its answer read as the loop's — and
// that cannot happen, because the memo is keyed by the pair and the day
// themselves (see rateKey), so a mis-enumerated rate is filed where nothing
// looks for it.
//
// The slice this builds names one query per TERM before it is returned —
// appendTermQueries asks once per lot, per income operation, per released
// parcel — so a position with many lots landing on a handful of purchase
// dates repeats the same (pair, day) many times over. That costs nothing at
// the database (RatesOn collapses duplicates before it ever queries the
// store), but it is not free: prewarmRates below walks this same slice again
// to file each answer in the memo, and RatesOn's own resolution walks it once
// more to build its result. Both of those passes are O(terms) unless this one
// hands them O(distinct queries) instead, so the return statement collapses
// the slice through dedupeQueries before handing it back — keyed with rateKey,
// the same identity the memo itself uses, rather than a second answer to what
// makes two queries "the same".
func rateQueries(
	positions map[uuid.UUID]*Position,
	instruments map[uuid.UUID]instrument.Instrument,
	quotes map[uuid.UUID]marketdata.Quote,
	income map[uuid.UUID][]Operation,
	cash map[string]*CashPosition,
	baseCurrency string,
	now time.Time,
) []marketdata.RateQuery {
	var out []marketdata.RateQuery
	// The money's own two questions, asked for every currency: what it is worth
	// today, and what each parcel still held was worth on the day it arrived
	// (see cashToAPI). Left out of this warm-up, each of them becomes a lookup
	// of its own inside the loop below — which is the N+1 the round-trip test
	// guards, and which caught exactly this omission.
	for currency, p := range cash {
		if currency == baseCurrency {
			continue
		}
		out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: now})
		for _, l := range p.Lots {
			out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: l.On})
		}
		// And the days its departures happened on, plus the days the parcels
		// they took had arrived — the two ends of the result already banked.
		for _, r := range p.Realizations {
			out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: r.OccurredOn})
			for _, l := range r.Released {
				out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: l.On})
			}
		}
	}
	for _, p := range positions {
		inst, known := instruments[p.InstrumentID]
		q, quoted := quotes[p.InstrumentID]
		if known {
			// marketValue's error is deliberately ignored here, as
			// operation.rateQueries ignores amountTerms': this pass only
			// predicts which rates to fetch, and the loop calls the same
			// function on the same position moments later and fails from the
			// place that knows which figure it was building (see toAPI). A
			// valuation that cannot be struck simply asks for no rate, which is
			// also all this function could usefully do about it.
			//
			// `quoted` is passed in rather than gating the call, exactly as in
			// toAPI: this must ask the same function the same question the loop
			// will, and marketValue answers `no quote` itself now.
			if _, currency, gap, _ := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q, quoted); gap == valuationStruck {
				if currency != p.Currency {
					// toAPI brings a valuation denominated in the face
					// currency into the position's own — the one lookup on
					// this page whose target is not the base currency.
					out = append(out, marketdata.RateQuery{From: currency, To: p.Currency, On: now})
				}
				if p.Currency != baseCurrency {
					// positionInBase carries that valuation on into the base
					// currency FROM THE SAME `currency` toAPI converted it out
					// of, not from the position's — the valuation is converted
					// once, from where it really is (#39, see positionInBase).
					// So this is `currency` here too, and both queries come out
					// of the one marketValue call above rather than restating
					// which currency a bond's valuation is in.
					//
					// It asks for today's rate only when there is a valuation
					// to convert. Whether there still is one depends on the
					// conversion just above having succeeded, which is not
					// known until it runs — so this asks whenever a valuation
					// exists at all, and over-asks in exactly the case where
					// the position ends up publishing no in_base valuation
					// anyway. It also asks when `currency` IS the base
					// currency, which positionInBase resolves as an identity
					// without touching the store: a wasted row in a batch that
					// was happening anyway, and the alternative — a second
					// place that knows identities are free — is how an
					// enumeration comes to disagree with its consumer.
					out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: now})
				}
			}
		}
		if p.Currency == baseCurrency {
			// Nothing else on this position converts: positionInBase and
			// realizedInBase both short-circuit on it.
			continue
		}
		if lots, dated := lotTerms(p.Lots, p.Currency); dated {
			out = appendTermQueries(out, lots, baseCurrency)
		}
		out = appendTermQueries(out, incomeTerms(income[p.InstrumentID]), baseCurrency)
		if realized, dated := realizedTerms(p.Realizations, p.Currency); dated {
			out = appendTermQueries(out, realized, baseCurrency)
		}
	}
	return dedupeQueries(out)
}

// appendTermQueries asks for the rate of every term's own date, out of every
// term's own currency — which is what sumInBase will do with the same terms,
// one at a time. Both halves come off the term itself, so a sum whose terms are
// in three currencies prefetches three pairs without this function knowing that
// income is the sum that does it.
func appendTermQueries(dst []marketdata.RateQuery, terms []datedMinor, to string) []marketdata.RateQuery {
	for _, t := range terms {
		dst = append(dst, marketdata.RateQuery{From: t.from, To: to, On: t.on})
	}
	return dst
}

// dedupeQueries collapses queries onto one entry per distinct (pair, day),
// keeping the first occurrence's own On value. It exists because
// appendTermQueries asks once per TERM rather than once per distinct date:
// 500 lots settling on 5 purchase dates produce 500 queries for 5 answers,
// and every one of the three passes rateQueries and its callers make over
// this slice — this function's own accumulation aside — pays for every
// repeat, not just the distinct ones the store ends up billed for.
//
// Filters in place (out := queries[:0]) rather than allocating a second
// slice: the read index is always at or ahead of the write index, so
// overwriting queries as it is walked never clobbers an element still to be
// read.
//
// Keyed with rateKey/newRateKey — the same identity the request's rate memo
// itself uses (see rateFor) — rather than a second, independent notion of
// "same query" that could disagree with it. That also sidesteps the usual
// time.Time hazard for free: two dates naming the same calendar day but
// differing in *time.Location or monotonic reading collapse here exactly as
// they already collapse in the memo, because newRateKey reduces both to the
// same YYYY-MM-DD string.
func dedupeQueries(queries []marketdata.RateQuery) []marketdata.RateQuery {
	seen := make(map[rateKey]bool, len(queries))
	out := queries[:0]
	for _, q := range queries {
		k := newRateKey(q.From, q.To, q.On)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, q)
	}
	return out
}

// prewarmRates resolves queries in one round trip and files each answer in the
// request's memo under the key rateFor will look it up by.
//
// NOTHING HERE FAILS THE REQUEST, and that is not laziness. This call buys
// speed, not truth: every figure below is struck by rateFor, which resolves
// whatever it does not find in the memo. A batch that fails leaves the memo
// empty and the screen is computed exactly as it was before any prefetch
// existed. An outage is met again by the very next lookup and reported from the
// code that knows which figure it was resolving and can tell a missing rate (a
// gap on the screen) from an outage (a 500). Failing here would move that
// judgement to a place that cannot make it, and would turn into an error page
// every request the fallback could have served correctly.
//
// WHICH ANSWERS GET FILED IS NOT DECIDED HERE. Rates.Answered walks the
// queries and hands back only the ones the batch resolved, so a query it never
// answered leaves no entry and rateFor resolves it itself, while a query
// answered with "no rate" arrives carrying marketdata.ErrNoRate and is filed as
// the honest gap it is. That rule is one statement for all three screens that
// warm a memo this way (see marketdata.Rates.Answered); only the key an answer
// is filed under is this package's own.
//
// The error is dropped here and reported one layer down. A failure specific to
// the BATCH statement — a timeout on the one large query, say — leaves the
// screen correct and slow, since rateFor resolves every figure per pair, so
// there is nothing for this handler to tell the user, and an error page would be
// a worse outcome than a slow one. But there IS something to tell whoever runs
// this: the optimization has stopped working and no request will ever say so.
// That warning is written where the batch actually dies, which is the only place
// all four survivors of such a failure pass through
// (marketdata.Converter.fetchRates, #70).
func (h *Handler) prewarmRates(ctx context.Context, queries []marketdata.RateQuery, cache map[rateKey]*rateLookup) {
	if len(queries) == 0 {
		return
	}
	resolved, err := h.conv.RatesOn(ctx, queries)
	if err != nil {
		return
	}
	for q, res := range resolved.Answered(queries) {
		cache[newRateKey(q.From, q.To, q.On)] = &rateLookup{rate: res.Rate, date: res.RateDate, err: res.Err}
	}
}
