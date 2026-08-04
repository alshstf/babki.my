import { useTranslation } from "react-i18next";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { formatMinor, formatPrice, formatPriceIn, signClass } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { resolveDisplayAmount } from "@/lib/display-amount";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import { MoneyCell } from "@/components/money-cell";
import { unnameableGap } from "@/lib/unnameable-gap";
import type { InBaseGap, MarketValueGap, Position } from "@/api/positions";

// The one sentence that captions EVERY money cell of a row, chosen from the
// term the server says it stopped on (Position.in_base_gap). It is the row's
// only source: has_undated_lots answers the same question for the first of
// these four causes and cannot disagree with it — both derive from one
// server-side predicate — but a caption assembled from two sources is a
// caption that will eventually be assembled from two sources that have drifted.
//
// Each sentence explains why the WHOLE row carries no base-currency figures,
// not why one cell does, and that is what makes it true over all four cells:
// in_base is published as a whole or not at all, so a single unvaluable term
// withholds the cost, the income, the profit AND the valuation together. A
// sentence that named only its own term — «нет курса на день покупки» — would
// hang over the income and the valuation, which need no purchase date, and
// that is exactly the defect (#66) this is fixing rather than repeating.
//
// The server checks its terms in a fixed order and stops at the first failure,
// so a named cause also asserts that nothing before it stopped the object; two
// of these sentences say so out loud («дело не в стоимости»), which is what
// answers the reader's obvious question about the ruble figure they can see is
// missing from a cell whose own rates are all there. They claim no more than
// that on purpose: a term the server never had to value — the cost of a
// position holding no lot, the income of one that received none — was not
// checked and found sound, it was simply never in the way, so «курсы на дни
// покупок нашлись» over a sum with no terms reported a lookup that never
// happened.
//
// Written as a switch over literal keys rather than a lookup table, so every
// key stays a literal at the t() call site — the only shape
// scripts/check-i18n.mjs can verify.
function rowGapTitle(t: (key: string) => string, gap: InBaseGap | null): string {
  switch (gap) {
    case "undated_lot":
      return t("positions.notConvertedUndatedLot");
    case "no_rate_lot_date":
      return t("positions.notConvertedNoRateLotDate");
    case "no_rate_income_date":
      return t("positions.notConvertedNoRateIncomeDate");
    case "no_rate_today":
      return t("positions.notConvertedNoRateToday");
    case null:
      // The contract publishes a cause whenever in_base is null and the
      // currency differs from the base one, so a row with a marker and no
      // cause should not occur. It is not left to crash or to say nothing:
      // the general phrase says only what the payload itself shows — the
      // base-currency figures were withheld — and it is what an older
      // server's payload deserves.
      return t("positions.notConverted");
    default:
      // The other way to reach that phrase, and the one that decided how it
      // is worded (#105). A server NEWER than this build sends a value outside
      // the union above; the sentence shown then may claim nothing about the
      // cause, because not knowing the cause is the very condition that brings
      // it here. «Нет курса», which is what it used to say, claims exactly
      // that — and would be false of a value shaped like `undated_lot`, which
      // is in TODAY's enum and is about a date nobody recorded rather than
      // about a rate. So a client one release behind a server that adds
      // another date-shaped cause would caption it «нет курса»: #66's defect,
      // reappearing through the path built to prevent it.
      return unnameableGap(gap, t("positions.notConverted"));
  }
}

// What the VALUATION cell says, which is the one cell that can have a cause of
// its own (Position.market_value_gap) — the nearer one, and the one that wins
// there. It answers for that cell in both of its states, and the two are not
// the same question, which is why `fallback` is a parameter rather than a
// constant here:
//
//   - the cell HAS a figure that could not be converted. The row's own
//     sentence is the fallback: when the valuation itself converted fine and
//     the row's term is what withheld the base-currency block, that sentence is
//     the true one and already says so. Repeating the valuation's own sentence
//     there would blame a third currency that is not in play; saying nothing
//     would leave an unexplained marker on a figure the reader can see is not
//     in rubles.
//   - the cell has NO figure and renders a dash. The fallback is then the
//     general «оценки нет, причина не названа», because the row's sentence is
//     about the base currency and this dash is not: a valuation this program
//     never struck is missing in every currency.
//
// The three no-figure causes are the whole of #78. Until it, this switch had
// one case and the dash was captioned «Нет котировки» from a literal in the
// markup — an inference from an absent figure, and false on two of the three
// rows it landed on: a crypto or metal position and a bond with no face value
// recorded can both carry a perfectly good quote. Their sentences must
// therefore not send the reader after one, and `type_not_priced`'s must not
// promise a figure at all: no decision to write such a valuation has been
// taken, so «пока» would be an invention.
//
// An unnameable value degrades to the caller's fallback for the same reason
// (#105): what is true of the cell is still true, the cause merely is not
// known, and a build one release behind a server must not answer a cause it
// cannot read with one it can.
function valuationGapTitle(
  t: (key: string) => string,
  gap: MarketValueGap | null,
  fallback: string,
): string {
  switch (gap) {
    case "no_quote":
      return t("positions.noQuote");
    case "type_not_priced":
      return t("positions.notPricedForType");
    case "no_face_value":
      return t("positions.noFaceValue");
    case "no_rate_valuation_currency":
      return t("positions.notConvertedValuationCurrency");
    case null:
      return fallback;
    default:
      return unnameableGap(gap, fallback);
  }
}

// Renders the price shown under the market value amount. The quote date used
// to be shown inline too ("274,49 · 28.07.2026") but that's visual noise for
// a detail nobody reads at a glance — it now lives in the row's `title`
// tooltip instead (see the caller). When the market value was converted from
// a different currency (e.g. a bond's face-value currency), the tooltip also
// names the original, unconverted amount — again tooltip-only, not shown as
// text, per the same "less visual noise" preference. Returns null unless
// both the price and the quote date are present and well-formed — a
// half-rendered hint would be more misleading than no hint at all.
//
// WHAT THE DATE IS, and why the tooltip spends two sentences on it. price_on
// is the trading session the SOURCE attaches this price to — never the day
// this program fetched it (see TickerQuote.On in
// internal/marketdata/provider.go; #90 was the fetch day being stored as the
// price's own, so on a Monday Friday's price was captioned with Monday). With
// the date now true, the date alone still is not: a bare «Цена на 31.07.2026»
// read on a Monday says both "this is the market's last word" and "the quotes
// job stopped a week ago", and nothing on this screen tells the two apart. So
// the caption states what the date is instead of leaving the reader to infer
// freshness the server never claimed.
//
// The wording is constrained by measurements against live ISS, all of them
// recorded in the QuotesFor doc block in internal/marketdata/moex/moex.go, and
// every constraint is a sentence this caption is forbidden to write:
//   - it is NOT the session's closing price. MOEX publishes that separately as
//     PREVLEGALCLOSEPRICE and the two are different numbers on the same row.
//   - it is NOT necessarily a price anything traded at that day. 779 of TQCB's
//     3021 rows in one measured session carried a price for a paper that did
//     not trade at all, so «сделки в тот день могло и не быть» is the fact,
//     and it is stated as a possibility because that is what was proven.
//   - it is NOT "the previous session" as far as THIS component can know. The
//     wire carries whatever day the source named; "previous" is a property of
//     MOEX's PREVPRICE, not of the contract this screen reads, and a caption
//     that asserted it would be a claim about a provider the frontend has
//     never heard of.
//   - it does NOT say the data is fresh. A date in the past is also exactly
//     what a broken refresh looks like, and reassurance here would cover for
//     one.
//
// The second sentence is the project's "three rates for three questions" rule
// made legible on this cell: the valuation IS struck from this price (see
// marketValue in internal/portfolio/http.go — every path that publishes
// Position.price publishes a valuation computed from it, and this hint is only
// rendered when there is one), while any conversion of that valuation is done
// at the current rate, never at the quote's date — toAPI converts into the
// position's currency and positionInBase into the base one, both at the
// request's `now`. Its conditional shape is load-bearing rather than timid: a
// position whose valuation is already in its own currency, viewed in native
// mode, converts nothing at all, and an unconditional «пересчитана по
// текущему курсу» would name a conversion that never happened. The rate's own
// DATE is deliberately not repeated here — MoneyCell prints it on the amount
// itself («Пересчитано по текущему курсу (на 20.07.2026)»), and two places
// stating one date is two places to drift apart.
//
// WHAT THE NUMBER IS depends on the instrument, and that is the whole of #32
// and #76 — one defect in two halves, a quote printed with nothing saying what
// its unit is.
//
// For a share or an ETF the quote is MONEY PER UNIT, in the currency the QUOTE
// is denominated in. That is normally the position's own, and nothing enforces
// it: Position.currency comes from the operation, a quote carries a currency
// column of its own, and where the two differ the server converts the
// VALUATION into the position's currency and discloses the original in
// market_value_source_currency — while Position.price stays exactly as quoted
// and is never converted. So the price's currency is that source field when it
// is set and market_value_currency otherwise, which is what the contract says
// in as many words (Position.price). BOTH readings are needed: on the ordinary
// row the two agree and either would do, and on the row where a quote is
// denominated in something other than the position's currency they do not, and
// market_value_currency then describes the CONVERTED figure above rather than
// this one.
//
// Naming it unconditionally, rather than only where the reader might be
// confused, is a decision (#76). The bare number was wrong in base mode — the
// valuation converts with the toggle and this line does not, so «274 950,00 ₽»
// stood over «305,50», which was dollars — and a mode-aware sign would be a
// caption whose truth depended on which toggle the reader last touched, silently
// wrong the moment he flipped it. The suffix has to exist for the mismatched-
// quote row anyway; having it appear and disappear as well would be two
// renderings of one number, one of them correct only in context.
//
// For a BOND the quote is a PERCENTAGE OF FACE VALUE (the MOEX convention):
// the server publishes q.Price untouched in Position.price, and marketValue()
// in internal/portfolio/http.go multiplies it as faceValueMinor × price/100 ×
// quantity. The demo seed's OFZ26238 makes the gap concrete — face value
// 1 000,00 ₽, quote 95.20, so one bond is worth 952 ₽ while the line under its
// ruble valuation reads "95,20". Bare, that is a money figure ten times too
// small, sitting under a money figure. It gets «%» and NO currency sign: a
// percentage is denominated in nothing, and both of the fields a share reads
// its currency from are filled in on a bond's row with the FACE value's
// currency and the position's, neither of which this number is in.
//
// The alternative fix — deriving the 952 ₽ and showing THAT — was rejected on
// two grounds. It is money arithmetic in the browser (face value × percent,
// with a rounding decision of its own), which this project does everywhere on
// the server; there is no per-unit price on the wire to render instead. And
// even done correctly it would replace the number the exchange actually
// quotes, the one the owner sees in his broker's app, with one no venue
// prints. So the quote stays as quoted, and says which unit it is in. Reading
// a currency CODE off the payload is not arithmetic and does not touch that
// rule: no figure is computed here, only labelled.
//
// Any other type keeps a bare number. Today that branch cannot be reached from
// this server — it prices share, etf and bond and nothing else, so a crypto or
// metal row carries no valuation, no price, and no hint at all (marketValue's
// `default`, published as market_value_gap: type_not_priced). It is written
// out rather than folded into the share/etf case because the day a newer
// server prices a new type, what its Position.price means will be decided
// there and not here, and a client one release behind must not answer that
// question with the valuation's currency. Same rule as the gap captions
// (#105): what is known stays said, what is not stays unsaid.
function priceHint(
  t: (key: string, opts?: Record<string, string>) => string,
  position: Position,
): { price: string; title: string } | null {
  if (!position.price || !position.price_on) return null;
  const formatted = formatPrice(position.price);
  // price_on, and nothing else on the position: it is the only field that
  // dates the PRICE. in_base.rate_on is a plausible-looking neighbour that
  // dates the valuation's fx conversion instead, and putting it here would
  // print a real date under a sentence about the wrong thing.
  const date = formatDate(position.price_on);
  if (formatted === null || !date) return null;
  let price = formatted;
  let title = t("positions.priceOn", { date }) + "\n" + t("positions.priceSession");
  // A switch over the instrument's type rather than "bond or else", so the
  // claim each branch makes is made only where it has been checked. Written
  // with literal keys at every t() call site — the only shape
  // scripts/check-i18n.mjs can verify.
  switch (position.instrument.type) {
    case "bond":
      price = t("positions.pricePercent", { price: formatted });
      title += "\n" + t("positions.priceIsPercentOfFace");
      break;
    case "share":
    case "etf": {
      const quoteCurrency =
        position.market_value_source_currency ?? position.market_value_currency;
      // The `?? formatted` is unreachable rather than defensive: this hint is
      // only built for a row that HAS a valuation, which is a row that has a
      // market_value_currency, and formatPrice already answered on this very
      // string. It falls back to the bare number all the same, because the
      // number is the position's own datum and dropping the line would hide
      // it to protect a sign.
      if (quoteCurrency) price = formatPriceIn(position.price, quoteCurrency) ?? formatted;
      break;
    }
  }
  title += "\n" + t("positions.priceValuationRate");
  const sourceCurrency = position.market_value_source_currency;
  const sourceMinor = position.market_value_source_minor;
  if (sourceCurrency != null && sourceMinor != null) {
    title += "\n" + t("positions.convertedFrom", { amount: formatMinor(sourceMinor, sourceCurrency) });
  }
  return { price, title };
}

// Formats the unrealized P&L as a percentage of cost ("+12,3 %" / "-12,3 %").
// This is a *display* ratio, not a money amount, so it's computed with plain
// number arithmetic here rather than routed through money.ts (per project
// convention, money.ts owns minor-unit amounts, not derived percentages).
// Returns null when cost is 0 — there is no honest percentage to show for a
// division by zero, so the caller omits the line entirely rather than
// display "Infinity %" or similarly nonsensical output.
function unrealizedPercent(unrealizedMinor: number, costMinor: number): string | null {
  if (costMinor === 0) return null;
  const ratio = unrealizedMinor / costMinor;
  return new Intl.NumberFormat("ru-RU", {
    style: "percent",
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
    signDisplay: "exceptZero",
  }).format(ratio);
}

export function PositionsTable({
  positions,
  mode,
  baseCurrency,
}: {
  positions: Position[];
  mode: DisplayCurrencyMode;
  // The space's base currency (Summary.base_currency) — needed to tell
  // "already in base, nothing to convert" apart from "conversion failed, no
  // fx rate" when a position's in_base is null (see resolveDisplayAmount).
  baseCurrency: string;
}) {
  const { t } = useTranslation();
  // A position row's money amounts are denominated in the position's own
  // currency, the quote's, or a bond face value's — never "the account's",
  // which is what the default MoneyCell wording says, so every cell below is
  // handed wording of its own.
  //
  // WHICH wording is not this screen's to work out. Five different things
  // leave a figure unconverted — four that stop the row's whole base-currency
  // block and one that stops its valuation alone — and they are five different
  // pieces of news to the person reading the row, one of them about a figure
  // that is never coming. Only the server knows which of them actually
  // happened, and it says so in Position.in_base_gap and
  // Position.market_value_gap; rowGapTitle and valuationGapTitle above turn
  // those answers into sentences and add nothing. Nothing here infers a cause
  // from a flag or from comparing two currency codes: an inferred cause is a
  // second answer waiting to disagree with the server's, and «нет курса» over
  // a row whose problem is a missing DATE is precisely the disagreement this
  // screen shipped with (#66).
  //
  // Both are per-row, hence resolved inside the map below.
  //
  // Only the market value is converted at today's rate, so only it keeps
  // MoneyCell's default "converted at the current rate (on <date>)" wording.
  // The ruble basis is built lot by lot at each purchase date's rate and
  // income operation by operation at each operation date's rate, so those
  // cells say which historical rates stand behind them instead.
  //
  // The date argument MoneyCell offers is deliberately ignored here:
  // in_base.rate_on is the rate date of the market VALUATION (that is what
  // the API contract says it is) and describes none of these three figures —
  // printing it under them would name a date that had no part in the number
  // above it.
  const costConvertedTitle = () => t("positions.convertedAtPurchaseRates");
  const incomeConvertedTitle = () => t("positions.convertedAtIncomeRates");
  const profitConvertedTitle = () => t("positions.convertedProfitMixed");
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("positions.columns.instrument")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.quantity")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.cost")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.market")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.profit")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.income")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {positions.map((position) => {
          const closed = position.quantity === "0";
          const unconvertedTitle = rowGapTitle(t, position.in_base_gap);
          // Market value's currency can differ from the position's own
          // currency (a bond's face-value currency, for instance), so it is
          // always formatted with market_value_currency, never `currency`.
          const marketValueMinor = position.market_value_minor;
          const marketValueCurrency = position.market_value_currency;
          const hasMarketValue = marketValueMinor != null && marketValueCurrency != null;
          // The valuation's own reason, which outranks the row's on its own
          // cell and nowhere else. The two can be set at once and both be
          // true — a row stopped by a dateless lot whose valuation is also
          // stuck in a third currency — and this cell takes the nearer of
          // them, while the rest of the row keeps the one that is true of it.
          //
          // WHICH FALLBACK depends on whether there is a figure in the cell,
          // and the two cases are answered by the same lookup so that one
          // vocabulary of causes reaches both. With a figure, an unnamed cause
          // means the row's own sentence is the true one. With none, the row's
          // sentence would be false — it is about the base currency, and a
          // valuation that was never struck is missing in every currency — so
          // the general "no valuation, cause not named" phrase stands instead.
          const valuationUnconvertedTitle = valuationGapTitle(
            t,
            position.market_value_gap,
            hasMarketValue ? unconvertedTitle : t("positions.noValuation"),
          );
          const hint = hasMarketValue ? priceHint(t, position) : null;
          const unrealizedMinor = position.unrealized_pnl_minor;
          const hasUnrealized = unrealizedMinor != null;
          // Cost is resolved unconditionally: it's always present (unlike
          // market value / unrealized P&L), and the profit percentage below
          // needs it alongside the resolved profit figure. Both numbers in
          // that ratio must live in the same currency, which the percentage
          // asserts for itself rather than assuming: in_base carries a null
          // unrealized_pnl_minor whenever the valuation could not be
          // expressed in the position's currency, so cost and profit do not
          // always resolve the same way.
          // One term of the row's converted block, welded to the currency that
          // block says its figures are in (PositionInBase.currency, required
          // by the contract) — never to the session's base currency, which is
          // a second answer to the same question and comes apart from this one
          // whenever a cached row outlives a change of base currency (#106).
          // The term is picked by a function of the block rather than passed
          // in, so a caller cannot hand over the position's OWN figure by
          // mistake and have it printed under the base currency's sign.
          const inBase = position.in_base;
          const convertedTerm = (
            term: (block: NonNullable<typeof inBase>) => number | null | undefined,
          ) =>
            inBase && {
              amountMinor: term(inBase),
              currency: inBase.currency,
              rateOn: inBase.rate_on,
            };
          const resolvedCost = resolveDisplayAmount(
            mode,
            position.currency,
            position.cost_minor,
            baseCurrency,
            convertedTerm((block) => block.cost_minor),
          );
          const resolvedMarketValue = hasMarketValue
            ? resolveDisplayAmount(
                mode,
                marketValueCurrency,
                marketValueMinor,
                baseCurrency,
                convertedTerm((block) => block.market_value_minor),
              )
            : null;
          const resolvedUnrealized = hasUnrealized
            ? resolveDisplayAmount(
                mode,
                position.currency,
                unrealizedMinor,
                baseCurrency,
                convertedTerm((block) => block.unrealized_pnl_minor),
              )
            : null;
          const resolvedIncome = resolveDisplayAmount(
            mode,
            position.currency,
            position.income_minor,
            baseCurrency,
            convertedTerm((block) => block.income_minor),
          );
          const unrealizedPct =
            resolvedUnrealized && resolvedUnrealized.currency === resolvedCost.currency
              ? unrealizedPercent(resolvedUnrealized.amountMinor, resolvedCost.amountMinor)
              : null;
          // A bare "+10,0 %" means a different thing per mode: in the
          // position's own currency it is the instrument's move alone, in the
          // base currency it carries the fx move too, so one position can
          // honestly read +10 % in one mode and -45 % in the other. Naming
          // the currency is what makes those two answers legible as answers
          // to different questions rather than as a discrepancy.
          //
          // The currency named is the one the ratio was actually computed in
          // (resolvedCost's — the guard above already proved the profit's
          // equal to it), not the one the mode asked for: in base mode with
          // no converted figure available both figures stay native, and the
          // label has to follow the numbers.
          const unrealizedPctTitle = t("positions.profitPercentIn", {
            currency: resolvedCost.currency,
          });
          return (
            <TableRow
              key={position.instrument.id}
              className={cn(closed && "opacity-50")}
            >
              <TableCell>
                <div className="font-medium">
                  {position.instrument.name}
                  {position.instrument.frozen && (
                    <Badge variant="outline" className="ml-2">
                      {t("positions.frozen")}
                    </Badge>
                  )}
                  {closed && (
                    <Badge variant="outline" className="ml-2">
                      {t("positions.closed")}
                    </Badge>
                  )}
                </div>
                <div className="text-xs text-muted-foreground">
                  {position.instrument.ticker}
                </div>
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {position.quantity}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                <MoneyCell
                  resolved={resolvedCost}
                  notConvertedTitle={unconvertedTitle}
                  convertedTitle={costConvertedTitle}
                  testId="position-cost"
                />
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {hasMarketValue && resolvedMarketValue ? (
                  <>
                    <MoneyCell
                      resolved={resolvedMarketValue}
                      notConvertedTitle={valuationUnconvertedTitle}
                      testId="position-market-value"
                    />
                    {hint && (
                      <div
                        data-testid="position-price"
                        className="text-xs font-normal text-muted-foreground"
                        title={hint.title}
                      >
                        {hint.price}
                      </div>
                    )}
                  </>
                ) : (
                  <span
                    data-testid="position-no-quote"
                    className="text-muted-foreground"
                    title={valuationUnconvertedTitle}
                  >
                    —
                  </span>
                )}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {resolvedUnrealized ? (
                  <>
                    <MoneyCell
                      resolved={resolvedUnrealized}
                      className={signClass(resolvedUnrealized.amountMinor)}
                      notConvertedTitle={unconvertedTitle}
                      convertedTitle={profitConvertedTitle}
                      testId="position-profit-amount"
                    />
                    {unrealizedPct && (
                      <div
                        data-testid="position-profit-percent"
                        className="text-xs font-normal text-muted-foreground"
                        title={unrealizedPctTitle}
                      >
                        {unrealizedPct}
                      </div>
                    )}
                  </>
                ) : (
                  <span
                    data-testid="position-profit-dash"
                    className="text-muted-foreground"
                    // Two ways to have no profit, and they are two different
                    // sentences. With a valuation present, the profit is
                    // missing because that valuation is in another currency
                    // and cannot be subtracted from the basis. With none, the
                    // profit is missing because one of its two operands is —
                    // so this cell says that in its own words and then hands
                    // over to the valuation's cause, whatever the server said
                    // it was. It used to print «Нет котировки» flat, which is
                    // the same false sentence #78 is about, one column over: a
                    // crypto row's profit is not waiting for a quote either.
                    title={
                      hasMarketValue
                        ? t("positions.currencyMismatch")
                        : t("positions.profitNeedsValuation") + "\n" + valuationUnconvertedTitle
                    }
                  >
                    —
                  </span>
                )}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                <MoneyCell
                  resolved={resolvedIncome}
                  notConvertedTitle={unconvertedTitle}
                  convertedTitle={incomeConvertedTitle}
                  testId="position-income"
                />
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
