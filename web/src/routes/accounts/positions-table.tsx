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
import type { Position } from "@/api/positions";

// Renders the "price · quote date" hint shown under the market value amount.
// Returns null (nothing rendered) unless both the price and the quote date
// are present and well-formed — a half-rendered hint would be more
// misleading than no hint at all.
function priceHint(
  t: (key: string, opts?: Record<string, string>) => string,
  position: Position,
): string | null {
  if (!position.price || !position.price_on) return null;
  const price = formatPrice(position.price);
  const date = formatDate(position.price_on);
  if (price === null || !date) return null;
  return t("positions.priceOn", { price, date });
}

export function PositionsTable({ positions }: { positions: Position[] }) {
  const { t } = useTranslation();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("positions.columns.instrument")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.quantity")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.cost")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.market")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.realized")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.income")}</TableHead>
          <TableHead className="text-right">{t("positions.columns.fees")}</TableHead>
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
                {formatMinor(position.cost_minor, position.currency)}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {hasMarketValue ? (
                  <>
                    <div data-testid="position-market-value">
                      {formatMinor(marketValueMinor, marketValueCurrency)}
                    </div>
                    {hint && (
                      <div className="text-xs font-normal text-muted-foreground">{hint}</div>
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
              <TableCell
                className={cn(
                  "text-right tabular-nums",
                  signClass(position.realized_pnl_minor),
                )}
              >
                {formatMinor(position.realized_pnl_minor, position.currency)}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {formatMinor(position.income_minor, position.currency)}
              </TableCell>
              <TableCell className="text-right tabular-nums text-muted-foreground">
                {formatMinor(position.fees_minor, position.currency)}
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
