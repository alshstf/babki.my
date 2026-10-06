import { useState, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
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
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  formatMinor,
  formatPrice,
  formatPriceIn,
  signClass,
} from "@/lib/money";
import { formatDate } from "@/lib/dates";
import {
  resolveDisplayAmount,
  resolveOptionalDisplayAmount,
} from "@/lib/display-amount";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import { MoneyCell } from "@/components/money-cell";
import { unnameableGap } from "@/lib/unnameable-gap";
import type {
  CashPosition,
  InBaseGap,
  MarketValueGap,
  Position,
} from "@/api/positions";
import type { PricedPaper } from "./purchase-price-dialog";
import type { QuotedPaper } from "./state-price-dialog";

// The sentence captioning every money cell of a row, from the term the server
// stopped on (Position.in_base_gap), the row's only source. Each explains why the
// whole row has no base-currency figures, since in_base is published whole or not
// at all; a sentence naming only its own term would be false over the other
// cells (#66). The server checks terms in order, so a named cause also says
// nothing earlier stopped it, and two sentences say so; they claim no more, since
// a term never valued was not "found sound". A switch with literal keys, the shape
// scripts/check-i18n.mjs can verify.
function rowGapTitle(
  t: (key: string) => string,
  gap: InBaseGap | null,
): string {
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
      // A null in_base with a different currency always carries a cause, so
      // this should not occur; the general phrase claims only what the payload
      // shows.
      return t("positions.notConverted");
    default:
      // A server newer than this build sent an unknown cause (#105). The phrase
      // then claims nothing about the cause: «нет курса» would be false of a
      // date-shaped cause like undated_lot (#66).
      return unnameableGap(gap, t("positions.notConverted"));
  }
}

// What the valuation cell says: its own cause (Position.market_value_gap) wins
// there. fallback differs by state:
//
//   - the cell has a figure that did not convert: the row's sentence, which is
//     then the true one;
//   - the cell has a dash: «оценки нет, причина не названа», since a valuation
//     never struck is missing in every currency.
//
// The three no-figure causes are #78: «Нет котировки» was false for crypto, metals
// and a bond without a face value, and type_not_priced must not promise a figure.
// An unknown cause falls back the same way (#105).
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

// STALE_PRICE_DAYS is how old a quote must be before the row shows its date
// instead of keeping it in the tooltip: past weekends, holidays and thinly traded
// bonds, and far short of the four years the owner's frozen funds have stood at
// (FXIT carries 420 280 ₽ of February-2022 money into the totals). Nothing is
// recomputed; only the age becomes visible.
const STALE_PRICE_DAYS = 30;

// staleSince returns the price's date when it is old enough to show, else
// null.
function staleSince(priceOn: string): string | null {
  const on = new Date(priceOn);
  if (Number.isNaN(on.getTime())) return null;
  const days = (Date.now() - on.getTime()) / 86_400_000;
  return days > STALE_PRICE_DAYS ? formatDate(priceOn) : null;
}

// priceHint is the line under the market value: the price, with its date and
// any unconverted original in the tooltip. Null unless price and date are
// well-formed.
//
// The date is the session the source attaches to the price (TickerQuote.On),
// never the fetch day (#90). The caption says what the date is without claiming
// freshness, and avoids what live ISS measurements (see QuotesFor in moex.go)
// forbid: it is not the closing price (PREVLEGALCLOSEPRICE is separate), not
// necessarily a traded price (779 of TQCB's 3021 rows had none that day), not
// "previous session" (a MOEX detail the contract does not carry), and not "fresh".
// The second sentence says the valuation is struck from this price and converted
// at the current rate, conditionally, since a native-currency row converts
// nothing; MoneyCell prints the rate date.
//
// What the number is depends on the instrument (#32, #76):
//
//   - share or ETF: money per unit in the quote's currency,
//     market_value_source_currency when set, else market_value_currency (the
//     contract's rule for Position.price), always named so the line does not
//     change meaning with the toggle;
//   - bond: a percentage of face («%», no currency), with the server's
//     price_money_minor beside it; no money arithmetic happens here;
//   - anything else: a bare number, since a newer server pricing a new type
//     decides what its price means (#105).
function priceHint(
  t: (key: string, opts?: Record<string, string>) => string,
  position: Position,
): { price: string; title: string } | null {
  if (!position.price || !position.price_on) return null;
  const formatted = formatPrice(position.price);
  // price_on, the only field dating the price; in_base.rate_on dates the
  // valuation's conversion instead.
  const date = formatDate(position.price_on);
  if (formatted === null || !date) return null;
  let price = formatted;
  let title =
    t("positions.priceOn", { date }) + "\n" + t("positions.priceSession");
  // One branch per type, so each claim is made only where checked; literal
  // t() keys for scripts/check-i18n.mjs.
  switch (position.instrument.type) {
    case "bond": {
      // Money first, the percent beside it: the percent is the market's quote,
      // but the row's other figures are money. The face value is a catalog
      // snapshot and drifts on an amortizing bond, as it already does in the
      // valuation. The money is the server's price_money_minor, rounded once
      // there.
      const perUnitMinor = position.price_money_minor;
      const faceCurrency = position.instrument.face_currency;
      const perUnit =
        perUnitMinor != null && faceCurrency
          ? formatMinor(perUnitMinor, faceCurrency)
          : null;
      price = perUnit
        ? t("positions.priceMoneyAndPercent", {
            money: perUnit,
            price: formatted,
          })
        : t("positions.pricePercent", { price: formatted });
      title += "\n" + t("positions.priceIsPercentOfFace");
      if (perUnit) title += "\n" + t("positions.priceMoneyFromFace");
      break;
    }
    case "share":
    case "etf": {
      const quoteCurrency =
        position.market_value_source_currency ?? position.market_value_currency;
      // Unreachable (a valued row has a market_value_currency), but the number
      // is shown even without its sign.
      if (quoteCurrency)
        price = formatPriceIn(position.price, quoteCurrency) ?? formatted;
      break;
    }
  }
  title += "\n" + t("positions.priceValuationRate");
  const sourceCurrency = position.market_value_source_currency;
  const sourceMinor = position.market_value_source_minor;
  if (sourceCurrency != null && sourceMinor != null) {
    title +=
      "\n" +
      t("positions.convertedFrom", {
        amount: formatMinor(sourceMinor, sourceCurrency),
      });
  }
  return { price, title };
}

// The income this position received in other currencies, each in its own
// currency, joined by "·"; null when none. income_minor is only the position
// currency's entry of income_by_currency, so a yuan bond paid in roubles reads 0
// there; this line shows the rest. Nothing is summed or converted. The server
// orders by currency code. The position's own currency is filtered out because
// income_minor already shows it.
function otherCurrencyIncome(position: Position): string | null {
  const others = position.income_by_currency.filter(
    (entry) => entry.currency !== position.currency,
  );
  if (others.length === 0) return null;
  return others
    .map((entry) => formatMinor(entry.income_minor, entry.currency))
    .join(" · ");
}

// Unrealized P&L as a percent of cost ("+12,3 %"). A display ratio, not money,
// so plain arithmetic is fine. Null at zero cost.
function unrealizedPercent(
  unrealizedMinor: number,
  costMinor: number,
): string | null {
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
  cash,
  mode,
  baseCurrency,
  onPriceUnknown,
  onStatePrice,
  instrumentLinks = false,
  rowLabel,
}: {
  positions: Position[];
  // Cash is a holding: bought at one rate, worth another today. Rows arrive
  // valued from the server, one per currency ever touched. Optional; absent and
  // empty render the same.
  cash?: CashPosition[];
  mode: DisplayCurrencyMode;
  // The space's base currency, to tell "nothing to convert" from "conversion
  // failed" when in_base is null (see resolveDisplayAmount).
  baseCurrency: string;
  // What «указать цену» beside a paper with no purchase price opens; absent
  // for a reader who cannot write.
  onPriceUnknown?: (paper: PricedPaper) => void;
  // What «указать цену» on an unquoted paper opens; absent for a reader who
  // cannot write.
  onStatePrice?: (paper: QuotedPaper) => void;
  // Whether a paper's name leads to its own page. Off where the table is drawn
  // outside the application's router.
  instrumentLinks?: boolean;
  // On a paper's page the first column names the account rather than the
  // paper; absent, it names the paper.
  rowLabel?: (position: Position) => { key: string; label: ReactNode };
}) {
  const { t } = useTranslation();
  // Every cell gets wording of its own: its figures are in the position's,
  // the quote's or a face currency, never "the account's". The cause comes from the
  // server (in_base_gap, market_value_gap) through rowGapTitle and
  // valuationGapTitle; nothing is inferred here (#66). Only the valuation is at
  // today's rate and keeps MoneyCell's default wording; cost and income name their
  // historical rates. MoneyCell's date argument is ignored: in_base.rate_on dates
  // the valuation only.
  const costConvertedTitle = () => t("positions.convertedAtPurchaseRates");
  // Each cell names the rates behind its own number: settled is all past
  // events at their own dates' rates; the total adds today's valuation at
  // today's rate.
  const settledConvertedTitle = () => t("positions.convertedSettled");
  const totalConvertedTitle = () => t("positions.convertedTotal");
  const profitConvertedTitle = () => t("positions.convertedProfitMixed");

  // Closed rows are hidden by default and counted beside the control. The
  // totals are the account's, over every position, and are not filtered with the
  // rows; the control says so.
  const [showClosed, setShowClosed] = useState(false);
  // Emptied currencies are hidden by the same control, so the control must
  // appear for them too.
  const emptiedCash = (cash ?? []).filter((c) => c.amount_minor === 0).length;
  const closedCount =
    positions.filter((p) => p.quantity === "0").length + emptiedCash;
  const shown = showClosed
    ? positions
    : positions.filter((p) => p.quantity !== "0");
  // An emptied currency is hidden with closed rows but not counted as
  // "closed".
  const shownCash = (cash ?? []).filter(
    (c) => showClosed || c.amount_minor !== 0,
  );

  return (
    <>
      {closedCount > 0 && (
        <div className="mb-2 flex flex-wrap items-center gap-2 text-sm">
          <Button
            variant="outline"
            size="sm"
            data-testid="toggle-closed-positions"
            onClick={() => setShowClosed((v) => !v)}
          >
            {showClosed ? t("positions.hideClosed") : t("positions.showClosed")}
          </Button>
          <span
            className="text-muted-foreground"
            data-testid="closed-positions-note"
          >
            {showClosed
              ? t("positions.closedShown", { count: closedCount })
              : t("positions.closedHidden", { count: closedCount })}
          </span>
        </div>
      )}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>
              {rowLabel ? t("positions.columns.account") : t("positions.columns.instrument")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.quantity")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.cost")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.market")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.profit")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.settled")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.total")}
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {shown.map((position) => {
            const closed = position.quantity === "0";
            const custom = rowLabel?.(position);
            const unconvertedTitle = rowGapTitle(t, position.in_base_gap);
            // The market value's currency may differ from the position's (a bond's
            // face currency), so it uses market_value_currency.
            const marketValueMinor = position.market_value_minor;
            const marketValueCurrency = position.market_value_currency;
            const hasMarketValue =
              marketValueMinor != null && marketValueCurrency != null;
            // The valuation's own cause outranks the row's on this cell only. The
            // fallback depends on whether there is a figure: with one, the row's
            // sentence; without, the general "no valuation" phrase.
            const valuationUnconvertedTitle = valuationGapTitle(
              t,
              position.market_value_gap,
              hasMarketValue ? unconvertedTitle : t("positions.noValuation"),
            );
            const hint = hasMarketValue ? priceHint(t, position) : null;
            const unrealizedMinor = position.unrealized_pnl_minor;
            const hasUnrealized = unrealizedMinor != null;
            // Cost is always present, and the profit percent needs it in the same
            // currency as the profit, which it checks: unrealized_pnl_minor can be
            // null in in_base when cost is not.
            //
            // termOf ties a converted term to PositionInBase.currency, never the
            // session's base currency, which a cached row can outlive (#106), and picks
            // the term from the block so a native figure cannot be printed under the
            // base sign.
            const inBase = position.in_base;
            const convertedTerm = (
              term: (
                block: NonNullable<typeof inBase>,
              ) => number | null | undefined,
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
            // The second income line is drawn when the cell above shows the
            // position's own figure, not by toggle mode: the converted figure already
            // includes every payment. The own figure appears when the toggle asks for
            // it, when the block could not be struck, or when the position is in the
            // base currency and has no block at all (a rouble paper paid a dollar
            // dividend).
            const otherIncome = resolvedIncome.converted
              ? null
              : otherCurrencyIncome(position);
            // These three can be missing in one currency and present in the other
            // (a disposal settled in a third currency), hence the optional resolver.
            const resolvedRealized = resolveOptionalDisplayAmount(
              mode,
              position.currency,
              position.realized_pnl_minor,
              baseCurrency,
              convertedTerm((block) => block.realized_pnl_minor),
            );
            const resolvedSettled = resolveOptionalDisplayAmount(
              mode,
              position.currency,
              position.settled_minor,
              baseCurrency,
              convertedTerm((block) => block.settled_minor),
            );
            const resolvedTotal = resolveOptionalDisplayAmount(
              mode,
              position.currency,
              position.total_minor,
              baseCurrency,
              convertedTerm((block) => block.total_minor),
            );
            // The settled figure's two terms, spelled out under it.
            const settledHint = resolvedSettled
              ? t("positions.settledHint", {
                  realized: resolvedRealized
                    ? formatMinor(
                        resolvedRealized.amountMinor,
                        resolvedRealized.currency,
                      )
                    : "—",
                  income: formatMinor(
                    resolvedIncome.amountMinor,
                    resolvedIncome.currency,
                  ),
                })
              : // WHICH OF THE TWO REASONS IT IS, ASKED OF THE DATA RATHER THAN
                // Why the realized figure is missing: a disposal settled in another
                // currency (realized_pnl_minor is null exactly then) or income in several
                // currencies. Told apart from published fields; the dash appears only when
                // the position-currency figure is missing.
                position.realized_pnl_minor == null
                ? t("positions.settledMissingSale")
                : t("positions.settledMissingIncome");
            const unrealizedPct =
              resolvedUnrealized &&
              resolvedUnrealized.currency === resolvedCost.currency
                ? unrealizedPercent(
                    resolvedUnrealized.amountMinor,
                    resolvedCost.amountMinor,
                  )
                : null;
            // The percent names its currency: in the position's currency it is the
            // instrument's move, in the base currency it includes the fx move. It
            // names the currency actually computed in, which can be native in base
            // mode.
            const unrealizedPctTitle = t("positions.profitPercentIn", {
              currency: resolvedCost.currency,
            });
            // Why the profit is empty, one string for tooltip and screen reader. With
            // a valuation, it is in another currency; without one, the cell says so
            // and defers to the valuation's cause (not a flat «Нет котировки», #78).
            const profitDashHint = hasMarketValue
              ? t("positions.currencyMismatch")
              : t("positions.profitNeedsValuation") +
                "\n" +
                valuationUnconvertedTitle;
            return (
              <TableRow
                key={custom?.key ?? position.instrument.id}
                className={cn(closed && "opacity-50")}
              >
                <TableCell>
                  <div className="font-medium">
                    {custom ? (
                      custom.label
                    ) : instrumentLinks ? (
                      <Link
                        to="/instruments/$instrumentId"
                        params={{ instrumentId: position.instrument.id }}
                        className="hover:underline"
                      >
                        {position.instrument.name}
                      </Link>
                    ) : (
                      position.instrument.name
                    )}
                    {!custom && position.instrument.frozen && (
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
                  {!custom && (
                    <div className="text-xs text-muted-foreground">
                      {position.instrument.ticker}
                    </div>
                  )}
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
                  {/* Shares that arrived with no purchase price count as bought for
                     nothing, so every profit on this row is overstated; said on the
                     paper, held or sold. */}
                  {position.has_unknown_cost && (
                    <div className="text-xs text-amber-600">
                      <span data-testid="position-unknown-cost" title={t("positions.unknownCostHint")}>
                        {t("positions.unknownCost")}
                      </span>
                      {onPriceUnknown && (
                        <>
                          {" · "}
                          <button
                            type="button"
                            data-testid="position-unknown-cost-action"
                            className="underline underline-offset-2 hover:text-amber-700"
                            onClick={() =>
                              onPriceUnknown({
                                id: position.instrument.id,
                                name: position.instrument.name,
                                ticker: position.instrument.ticker,
                              })
                            }
                          >
                            {t("positions.unknownCostAction")}
                          </button>
                        </>
                      )}
                    </div>
                  )}
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
                      {/* A stale quote is still what the valuation and totals use, so its
                         age is shown on the row. */}
                      {position.price_by_hand && (
                        <div data-testid="position-price-by-hand" className="text-xs text-muted-foreground">
                          {t("positions.priceByHand")}
                          {onStatePrice && (
                            <>
                              {" · "}
                              <button
                                type="button"
                                className="underline underline-offset-2"
                                onClick={() =>
                                  onStatePrice({
                                    id: position.instrument.id,
                                    name: position.instrument.name,
                                    currency: position.instrument.currency,
                                    bond: position.instrument.type === "bond",
                                  })
                                }
                              >
                                {t("positions.statePriceAgain")}
                              </button>
                            </>
                          )}
                        </div>
                      )}
                      {position.price_source === "nav" && (
                        <div data-testid="position-price-nav" className="text-xs text-muted-foreground" title={t("positions.priceNavHint")}>
                          {t("positions.priceNav")}
                        </div>
                      )}
                      {position.price_source === "foreign" && (
                        <div data-testid="position-price-foreign" className="text-xs text-muted-foreground" title={t("positions.priceForeignHint")}>
                          {t("positions.priceForeign")}
                        </div>
                      )}
                      {/* What can be sold now, beside the full valuation above (decision
                         Р-11): the «Итого» counts this one. */}
                      {position.quantity !== "0" &&
                        (position.liquid_value_minor == null ? (
                          <div data-testid="position-not-traded" className="text-xs text-amber-600" title={t("positions.notTradedHint")}>
                            {position.last_traded_on
                              ? t("positions.notTradedSince", { date: formatDate(position.last_traded_on) })
                              : t("positions.notTraded")}
                          </div>
                        ) : (
                          position.liquid_value_minor !== position.market_value_minor && (
                            <div data-testid="position-liquid" className="text-xs text-muted-foreground" title={t("positions.liquidHint")}>
                              {t("positions.liquid", {
                                amount:
                                  mode === "base" && position.liquid_value_in_base_minor != null
                                    ? formatMinor(position.liquid_value_in_base_minor, baseCurrency)
                                    : formatMinor(position.liquid_value_minor, position.currency),
                              })}
                            </div>
                          )
                        ))}
                      {/* «Не торгуется с …» already dates a stale market price. */}
                      {position.price_on && staleSince(position.price_on) && position.last_traded_on !== position.price_on && (
                        <div
                          data-testid="position-price-stale"
                          className="text-xs text-amber-600"
                          title={t("positions.priceStaleHint")}
                        >
                          {t("positions.priceStale", {
                            date: staleSince(position.price_on) as string,
                          })}
                        </div>
                      )}
                    </>
                  ) : (
                    <span
                      data-testid="position-no-quote"
                      className="text-muted-foreground"
                      title={valuationUnconvertedTitle}
                    >
                      {/* The dash is hidden from assistive technology and the sentence
                         beside it is read (#31). */}
                      <span aria-hidden="true">—</span>
                      <span className="sr-only">
                        {valuationUnconvertedTitle}
                      </span>
                      {onStatePrice && position.market_value_gap === "no_quote" && (
                        <button
                          type="button"
                          data-testid="position-state-price"
                          className="block text-xs text-amber-600 underline underline-offset-2"
                          onClick={() =>
                            onStatePrice({
                              id: position.instrument.id,
                              name: position.instrument.name,
                              currency: position.instrument.currency,
                              bond: position.instrument.type === "bond",
                            })
                          }
                        >
                          {t("positions.statePrice")}
                        </button>
                      )}
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
                      title={profitDashHint}
                    >
                      <span aria-hidden="true">—</span>
                      <span className="sr-only">{profitDashHint}</span>
                    </span>
                  )}
                  {/* The realized result, under the unrealized one rather than
                     instead of it: one cell, one figure, one caption. Drawn only
                     when there is one. */}
                  {resolvedRealized && resolvedRealized.amountMinor !== 0 && (
                    <div
                      data-testid="position-realized"
                      className="text-xs font-normal text-muted-foreground"
                      title={t("positions.realizedHintRow")}
                    >
                      {t("positions.realizedOnRow", {
                        amount: formatMinor(
                          resolvedRealized.amountMinor,
                          resolvedRealized.currency,
                        ),
                      })}
                    </div>
                  )}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  title={settledHint}
                >
                  {resolvedSettled ? (
                    <MoneyCell
                      resolved={resolvedSettled}
                      notConvertedTitle={unconvertedTitle}
                      convertedTitle={settledConvertedTitle}
                      testId="position-settled"
                    />
                  ) : (
                    <span
                      data-testid="position-settled-dash"
                      className="text-muted-foreground"
                    >
                      <span aria-hidden="true">—</span>
                      <span className="sr-only">{settledHint}</span>
                    </span>
                  )}
                  {otherIncome && (
                    <div
                      data-testid="position-income-other-currency"
                      className="text-xs font-normal text-muted-foreground"
                      title={t("positions.incomeOtherCurrencyHint")}
                    >
                      {t("positions.incomeOtherCurrency", {
                        amounts: otherIncome,
                      })}
                    </div>
                  )}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  title={t("positions.totalHint")}
                >
                  {resolvedTotal ? (
                    <MoneyCell
                      resolved={resolvedTotal}
                      notConvertedTitle={unconvertedTitle}
                      convertedTitle={totalConvertedTitle}
                      testId="position-total"
                    />
                  ) : (
                    <span
                      data-testid="position-total-dash"
                      className="text-muted-foreground"
                    >
                      <span aria-hidden="true">—</span>
                      <span className="sr-only">
                        {t("positions.totalMissing")}
                      </span>
                    </span>
                  )}
                </TableCell>
              </TableRow>
            );
          })}
          {shownCash.map((money) => {
            // In native mode cash is itself (a thousand yuan cost a thousand yuan),
            // so only the balance is shown; in base mode it has a cost, value and
            // profit like any holding.
            const inBase = money.in_base;
            const showInBase =
              mode === "base" && money.currency !== baseCurrency;
            return (
              <TableRow key={`cash-${money.currency}`} data-testid="cash-row">
                <TableCell>
                  <div className="font-medium" data-testid="cash-currency">
                    {t("positions.cashName", { currency: money.currency })}
                  </div>
                  <div className="text-xs text-muted-foreground">
                    {t("positions.cashKind")}
                  </div>
                  {/* An overdraft has no gain to publish; the cell says the journal is
                     missing operations rather than staying empty. */}
                  {money.amount_minor < 0 && (
                    <div
                      data-testid="cash-overdraft"
                      className="text-xs text-amber-600"
                      title={t("positions.cashOverdraftHint")}
                    >
                      {t("positions.cashOverdraft")}
                    </div>
                  )}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  data-testid="cash-amount"
                >
                  {formatMinor(money.amount_minor, money.currency)}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  data-testid="cash-cost"
                >
                  {showInBase && inBase.cost_minor != null
                    ? formatMinor(inBase.cost_minor, inBase.currency)
                    : ""}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  data-testid="cash-value"
                >
                  {showInBase && inBase.value_minor != null
                    ? formatMinor(inBase.value_minor, inBase.currency)
                    : ""}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  title={showInBase ? t("positions.cashProfitHint") : undefined}
                >
                  {/* An empty cell can mean an overdraft (noted under the name) or a
                     missing rate, which has nowhere else to appear (gold under a
                     currency the Bank of Russia does not quote). */}
                  {showInBase &&
                    inBase.unrealized_pnl_minor == null &&
                    money.amount_minor >= 0 && (
                      <span
                        data-testid="cash-no-rate"
                        className="text-muted-foreground"
                        title={t("positions.cashNoRateHint")}
                      >
                        {t("positions.cashNoRate")}
                      </span>
                    )}
                  {showInBase && inBase.unrealized_pnl_minor != null ? (
                    <span
                      data-testid="cash-profit"
                      className={signClass(inBase.unrealized_pnl_minor)}
                    >
                      {formatMinor(
                        inBase.unrealized_pnl_minor,
                        inBase.currency,
                      )}
                    </span>
                  ) : (
                    ""
                  )}
                  {/* What this money already earned, under what it earns now, as on a
                     paper's row. Drawn only when there is one. */}
                  {showInBase &&
                    inBase.realized_pnl_minor != null &&
                    inBase.realized_pnl_minor !== 0 && (
                      <div
                        data-testid="cash-realized"
                        className="text-xs font-normal text-muted-foreground"
                        title={t("positions.cashRealizedHint")}
                      >
                        {t("positions.cashRealizedOnRow", {
                          amount: formatMinor(
                            inBase.realized_pnl_minor,
                            inBase.currency,
                          ),
                        })}
                      </div>
                    )}
                </TableCell>
                {/* Settled and total belong to papers; left empty, not noughts that
                   would read as figures. */}
                <TableCell />
                <TableCell />
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </>
  );
}
