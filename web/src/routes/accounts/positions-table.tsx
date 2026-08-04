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
import { formatMinor, formatPrice, signClass } from "@/lib/money";
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
      // the general phrase is vague but true — some rate is missing
      // somewhere — and it is what an older server's payload deserves.
      return t("positions.notConverted");
    default:
      return unnameableGap(gap, t("positions.notConverted"));
  }
}

// What the VALUATION cell says, which is the one cell that can have a cause of
// its own (Position.market_value_gap) — the nearer one, and the one that wins
// there. `rowTitle` is what it falls back to, and that fallback is the whole
// decision this cell needed: when the valuation itself converted fine and the
// row's own term is what withheld it, the row's sentence is the true one and
// it already says so. Repeating the valuation's own sentence there would blame
// a third currency that is not in play; saying nothing would leave an
// unexplained marker on a figure the reader can see is not in rubles.
//
// An unnameable value degrades to the row's sentence for the same reason: the
// row's cause is true of this cell too (the object it withholds contains this
// figure), it is merely not the nearest one — and where the row has no named
// cause either, that fallback is itself the general phrase.
function valuationGapTitle(
  t: (key: string) => string,
  gap: MarketValueGap | null,
  rowTitle: string,
): string {
  switch (gap) {
    case "no_rate_valuation_currency":
      return t("positions.notConvertedValuationCurrency");
    case null:
      return rowTitle;
    default:
      return unnameableGap(gap, rowTitle);
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
// WHAT THE NUMBER IS depends on the instrument, and that is the whole of #32.
// For a share or an ETF the quote is money per unit, in the currency the
// QUOTE is denominated in. That is normally the position's own, and nothing
// enforces it: Position.currency comes from the operation, a quote carries a
// currency column of its own, and where the two differ the server converts
// the VALUATION into the position's currency and discloses that in
// market_value_source_currency — while Position.price stays exactly as
// quoted and is never converted. On the ordinary row where they agree, the
// bare number matches the valuation above it and needs no unit stated, in
// the position's own-currency display mode. Two things break that, and
// neither is a caption problem: in BASE mode the valuation converts and this
// price line does not, so a foreign share prints a bare number in a currency
// the ruble amount above it is not in; and a share quoted in a currency
// other than its position's — which no seed row and no test exercises today
// — would print one in either mode. Both want a different RENDERING (a unit
// suffix or a mode-aware omission) rather than a truer caption, so they stay
// separate, still-open cases rather than something this comment explains away.
// For a BOND the quote is a percentage of face value (the MOEX convention):
// the server publishes q.Price untouched in Position.price, and
// marketValue() in internal/portfolio/http.go multiplies it as
// faceValueMinor × price/100 × quantity. The demo seed's OFZ26238 makes the
// gap concrete — face value 1 000,00 ₽, quote 95.20, so one bond is worth
// 952 ₽ while the line under its ruble valuation reads "95,20". Bare, that is
// a money figure ten times too small, sitting under a money figure.
//
// The alternative fix — deriving the 952 ₽ and showing THAT — was rejected on
// two grounds. It is money arithmetic in the browser (face value × percent,
// with a rounding decision of its own), which this project does everywhere on
// the server; there is no per-unit price on the wire to render instead. And
// even done correctly it would replace the number the exchange actually
// quotes, the one the owner sees in his broker's app, with one no venue
// prints. So the quote stays as quoted, and says which unit it is in.
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
  // Written as a literal-key branch rather than t(cond ? a : b) so both keys
  // stay verifiable by scripts/check-i18n.mjs, which only reads literals.
  if (position.instrument.type === "bond") {
    price = t("positions.pricePercent", { price: formatted });
    title += "\n" + t("positions.priceIsPercentOfFace");
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
          const valuationUnconvertedTitle = valuationGapTitle(
            t,
            position.market_value_gap,
            unconvertedTitle,
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
          const resolvedCost = resolveDisplayAmount(
            mode,
            position.currency,
            position.cost_minor,
            baseCurrency,
            position.in_base?.cost_minor,
            position.in_base?.rate_on,
          );
          const resolvedMarketValue = hasMarketValue
            ? resolveDisplayAmount(
                mode,
                marketValueCurrency,
                marketValueMinor,
                baseCurrency,
                position.in_base?.market_value_minor,
                position.in_base?.rate_on,
              )
            : null;
          const resolvedUnrealized = hasUnrealized
            ? resolveDisplayAmount(
                mode,
                position.currency,
                unrealizedMinor,
                baseCurrency,
                position.in_base?.unrealized_pnl_minor,
                position.in_base?.rate_on,
              )
            : null;
          const resolvedIncome = resolveDisplayAmount(
            mode,
            position.currency,
            position.income_minor,
            baseCurrency,
            position.in_base?.income_minor,
            position.in_base?.rate_on,
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
          // no fx rate both figures stay native, and the label has to follow
          // the numbers.
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
                    title={t("positions.noQuote")}
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
                    title={t(hasMarketValue ? "positions.currencyMismatch" : "positions.noQuote")}
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
