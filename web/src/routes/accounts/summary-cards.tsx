import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { formatMinorCompact } from "@/lib/money";
import { cn } from "@/lib/utils";
import { signClass } from "@/lib/money";
import { formatDate, isRecent, localToday } from "@/lib/dates";
import type { Summary } from "@/api/accounts";

// Context line under the total: honesty over silence — a null total or an
// unconverted currency is explained, never swept under a "0". Both pieces
// can appear together, joined by " · ".
function contextText(
  t: (key: string, opts?: Record<string, string>) => string,
  summary: Summary,
): string {
  const parts: string[] = [];
  if (summary.unconverted.length > 0) {
    parts.push(t("summary.unconverted", { currencies: summary.unconverted.join(", ") }));
  }
  if (summary.rates_on && !isRecent(summary.rates_on, localToday())) {
    parts.push(t("summary.ratesOn", { date: formatDate(summary.rates_on) }));
  }
  return parts.join(" · ");
}

export function SummaryCards({ summary }: { summary: Summary }) {
  const { t } = useTranslation();
  if (summary.totals.length === 0) return null;
  const totalInBase = summary.total_in_base_minor;
  const hasTotal = totalInBase !== null && totalInBase !== undefined;
  const context = contextText(t, summary);
  return (
    <div className="grid gap-4">
      <Card className={cn(!hasTotal && "opacity-70")}>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm font-medium text-muted-foreground">
            {t("summary.totalTitle")}
          </CardTitle>
        </CardHeader>
        <CardContent className="grid gap-1">
          {hasTotal ? (
            <div
              data-testid="summary-total-amount"
              className={cn("text-3xl font-bold", signClass(totalInBase))}
            >
              {formatMinorCompact(totalInBase, summary.base_currency)}
            </div>
          ) : (
            <div className="text-lg font-medium text-muted-foreground">{t("summary.noTotal")}</div>
          )}
          {context && <div className="text-xs text-muted-foreground">{context}</div>}
        </CardContent>
      </Card>
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {summary.totals.map((total) => (
          <Card key={total.currency}>
            <CardHeader className="pb-2">
              <CardTitle className="text-sm font-medium text-muted-foreground">
                {t("accounts.totalIn", { currency: total.currency })}
              </CardTitle>
            </CardHeader>
            <CardContent className="grid gap-1">
              <div className={cn("text-2xl font-bold", signClass(total.net_minor))}>
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
