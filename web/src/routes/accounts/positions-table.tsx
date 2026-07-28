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
          // Market value's currency can differ from the position's own
          // currency (a bond's face-value currency, for instance), so it is
          // always formatted with market_value_currency, never `currency`.
          const marketValueMinor = position.market_value_minor;
          const marketValueCurrency = position.market_value_currency;
          const hasMarketValue = marketValueMinor != null && marketValueCurrency != null;
          const hint = hasMarketValue ? priceHint(t, position) : null;
          const unrealizedMinor = position.unrealized_pnl_minor;
          const hasUnrealized = unrealizedMinor != null;
          // Cost is resolved unconditionally: it's always present (unlike
          // market value / unrealized P&L), and the profit percentage below
          // needs it alongside the resolved profit figure so both numbers
          // in that ratio are consistently either both native or both
          // converted (see resolveDisplayAmount — cost and unrealized_pnl
          // share the same in_base object, so they always resolve the same
          // way for a given position).
          const resolvedCost = resolveDisplayAmount(
            mode,
            position.currency,
            position.cost_minor,
            baseCurrency,
            position.in_base?.cost_minor,
          );
          const resolvedMarketValue = hasMarketValue
            ? resolveDisplayAmount(
                mode,
                marketValueCurrency,
                marketValueMinor,
                baseCurrency,
                position.in_base?.market_value_minor,
              )
            : null;
          const resolvedUnrealized = hasUnrealized
            ? resolveDisplayAmount(
                mode,
                position.currency,
                unrealizedMinor,
                baseCurrency,
                position.in_base?.unrealized_pnl_minor,
              )
            : null;
          const resolvedIncome = resolveDisplayAmount(
            mode,
            position.currency,
            position.income_minor,
            baseCurrency,
            position.in_base?.income_minor,
          );
          const unrealizedPct = resolvedUnrealized
            ? unrealizedPercent(resolvedUnrealized.amountMinor, resolvedCost.amountMinor)
            : null;
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
                <MoneyCell resolved={resolvedCost} testId="position-cost" />
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {hasMarketValue && resolvedMarketValue ? (
                  <>
                    <MoneyCell resolved={resolvedMarketValue} testId="position-market-value" />
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
                      testId="position-profit-amount"
                    />
                    {unrealizedPct && (
                      <div
                        data-testid="position-profit-percent"
                        className="text-xs font-normal text-muted-foreground"
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
                <MoneyCell resolved={resolvedIncome} testId="position-income" />
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
