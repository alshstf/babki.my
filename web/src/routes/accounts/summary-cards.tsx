import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { formatMinorCompact } from "@/lib/money";
import { cn } from "@/lib/utils";
import { signClass } from "@/lib/money";
import type { Summary } from "@/api/accounts";

export function SummaryCards({ summary }: { summary: Summary }) {
  const { t } = useTranslation();
  if (summary.totals.length === 0) return null;
  return (
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
                  {t("accounts.liabilities")}: {formatMinorCompact(total.liabilities_minor, total.currency)}
                </>
              )}
            </div>
          </CardContent>
        </Card>
      ))}
    </div>
  );
}
