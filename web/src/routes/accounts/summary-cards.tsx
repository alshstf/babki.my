import { useTranslation } from "react-i18next";
import { Info } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { formatMinorCompact } from "@/lib/money";
import { cn } from "@/lib/utils";
import { signClass } from "@/lib/money";
import { formatDate, isRecent, localToday } from "@/lib/dates";
import type { Summary } from "@/api/accounts";

// The "unconverted currencies" warning is an honesty disclosure, not visual
// noise: it stays as text in the card, unlike the rates date (see
// staleRatesTitle below), which moves into a tooltip.
function unconvertedText(
  t: (key: string, opts?: Record<string, string>) => string,
  summary: Summary,
): string {
  if (summary.unconverted.length === 0) return "";
  return t("summary.unconverted", { currencies: summary.unconverted.join(", ") });
}

// Returns the tooltip title for the stale-rates indicator icon, or null when
// the rates are fresh (isRecent) or the date is missing/unparseable — in
// those cases no icon is shown at all, per the "silence over noise" rule.
function staleRatesTitle(
  t: (key: string, opts?: Record<string, string>) => string,
  summary: Summary,
): string | null {
  if (!summary.rates_on || isRecent(summary.rates_on, localToday())) return null;
  const formattedDate = formatDate(summary.rates_on);
  if (!formattedDate) return null;
  return t("summary.ratesOn", { date: formattedDate });
}

export function SummaryCards({ summary }: { summary: Summary }) {
  const { t } = useTranslation();
  if (summary.totals.length === 0) return null;
  const totalInBase = summary.total_in_base_minor;
  const hasTotal = totalInBase !== null && totalInBase !== undefined;
  const unconverted = unconvertedText(t, summary);
  const ratesTitle = staleRatesTitle(t, summary);
  return (
    <div className="grid gap-4">
      {/* Accent card: the total is the headline number, so it gets a large
          size and a visible border/tint — everything below is secondary. */}
      <Card className={cn("border-primary/40 bg-accent/30", !hasTotal && "opacity-70")}>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm font-medium text-muted-foreground">
            {t("summary.totalTitle")}
          </CardTitle>
        </CardHeader>
        <CardContent className="grid gap-1">
          {hasTotal ? (
            <div className="flex items-center gap-2">
              <div
                data-testid="summary-total-amount"
                className={cn("text-4xl font-bold", signClass(totalInBase))}
              >
                {formatMinorCompact(totalInBase, summary.base_currency)}
              </div>
              {ratesTitle && (
                // `title` goes on the wrapping <span>, not the <Info> icon
                // itself — SVGProps in React's types doesn't include the
                // global `title` attribute even though browsers honor it, so
                // this sidesteps the type error while keeping the same
                // native-tooltip behavior used elsewhere (e.g. the no-quote
                // dash in positions-table.tsx).
                <span
                  data-testid="summary-rates-stale-icon"
                  className="inline-flex shrink-0 text-muted-foreground"
                  title={ratesTitle}
                >
                  <Info size={14} />
                </span>
              )}
            </div>
          ) : (
            <div className="text-lg font-medium text-muted-foreground">{t("summary.noTotal")}</div>
          )}
          {unconverted && <div className="text-xs text-muted-foreground">{unconverted}</div>}
        </CardContent>
      </Card>
      {/* Per-currency breakdown: compact, secondary — not equal weight to
          the total above. */}
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {summary.totals.map((total) => (
          <Card key={total.currency} size="sm">
            <CardHeader className="pb-1">
              <CardTitle className="text-xs font-medium text-muted-foreground">
                {t("accounts.totalIn", { currency: total.currency })}
              </CardTitle>
            </CardHeader>
            <CardContent className="grid gap-0.5">
              <div className={cn("text-base font-semibold", signClass(total.net_minor))}>
                {formatMinorCompact(total.net_minor, total.currency)}
              </div>
              <div className="text-xs text-muted-foreground">
                {t("accounts.assets")}: {formatMinorCompact(total.assets_minor, total.currency)}
                {total.liabilities_minor !== 0 && (
                  <>
                    {" · "}
                    {t("accounts.liabilities")}:{" "}
                    {formatMinorCompact(total.liabilities_minor, total.currency)}
                  </>
                )}
              </div>
            </CardContent>
          </Card>
        ))}
      </div>
    </div>
  );
}
