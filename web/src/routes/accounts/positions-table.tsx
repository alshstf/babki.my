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
import type { Position } from "@/api/positions";

// Renders the price shown under the market value amount. The quote date used
// to be shown inline too ("274,49 · 28.07.2026") but that's visual noise for
// a detail nobody reads at a glance — it now lives in the row's `title`
// tooltip instead (see the caller). When the market value was converted from
// a different currency (e.g. a bond's face-value currency), the tooltip also
// names the original, unconverted amount — again tooltip-only, not shown as
// text, per the same "less visual noise" preference. Returns null unless
// both the price and the quote date are present and well-formed — a
// half-rendered hint would be more misleading than no hint at all.
function priceHint(
  t: (key: string, opts?: Record<string, string>) => string,
  position: Position,
): { price: string; title: string } | null {
  if (!position.price || !position.price_on) return null;
  const price = formatPrice(position.price);
  const date = formatDate(position.price_on);
  if (price === null || !date) return null;
  let title = t("positions.priceOn", { date });
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
  // which is what the default MoneyCell wording says. Resolved once here and
  // handed to every cell rather than per cell.
  //
  // Three different conditions leave a figure unconverted, and they are not
  // the same news to the person reading it. Each says what is missing and
  // what is shown instead, in that order — the shape every "this is not in
  // the base currency" sentence in this application follows.
  //
  // A missing fx rate is a gap the instance's own backfill closes on its own
  // — the ruble figure appears later.
  //
  // A lot that does not know when it was bought (has_undated_lots, see the
  // API contract) never converts: the purchase date was never recorded and
  // nothing can recover it, so there is no rate to ask for. Saying "нет
  // курса" over that names a cause that is not the cause and promises a
  // number that will never come.
  //
  // A valuation that came out in a THIRD currency — a bond priced off a face
  // value denominated in neither the position's currency nor the base one —
  // is the third, and "нет курса" is wrong about it in a subtler way: a rate
  // from that currency to the base one may well exist. What is missing is the
  // link from it to the position's currency, without which the figure cannot
  // be brought into the base currency at all; multiplying it by the
  // position's own rate would be a silently wrong number (see
  // PositionInBase.market_value_minor). The cause is the chain, not the rate,
  // and only this figure has it — the row's cost and income are in the
  // position's currency and convert as usual.
  //
  // All three are per-row, hence resolved inside the map below.
  const notConvertedTitle = t("positions.notConverted");
  const undatedLotTitle = t("positions.notConvertedUndatedLot");
  const valuationCurrencyTitle = t("positions.notConvertedValuationCurrency");
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
          const unconvertedTitle = position.has_undated_lots
            ? undatedLotTitle
            : notConvertedTitle;
          // Market value's currency can differ from the position's own
          // currency (a bond's face-value currency, for instance), so it is
          // always formatted with market_value_currency, never `currency`.
          const marketValueMinor = position.market_value_minor;
          const marketValueCurrency = position.market_value_currency;
          const hasMarketValue = marketValueMinor != null && marketValueCurrency != null;
          // The valuation's own reason, which outranks the row's: when it is
          // denominated in some third currency, no chain reaches the base one
          // from it, and that stays true whether or not the row also has a
          // dateless lot or a missing rate of its own. The other cells keep
          // the row's reason — it is the only one true of them.
          const valuationUnconvertedTitle =
            hasMarketValue && marketValueCurrency !== position.currency
              ? valuationCurrencyTitle
              : unconvertedTitle;
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
