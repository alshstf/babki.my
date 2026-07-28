import { Info } from "lucide-react";
import { useTranslation } from "react-i18next";
import { formatMinor } from "@/lib/money";
import { cn } from "@/lib/utils";
import type { ResolvedAmount } from "@/lib/display-amount";

// Renders one already-resolved money amount (see resolveDisplayAmount in
// lib/display-amount.ts): the formatted number, plus — only when
// `resolved.noRate` — a small tooltipped indicator explaining that this
// figure could not be converted (no fx rate was available) and is shown in
// its native currency instead. Shared by every table/detail cell that shows
// a money amount alongside an optional in_base/balance_in_base
// counterpart, so this branching and markup lives in exactly one place
// instead of being repeated per screen.
export function MoneyCell({
  resolved,
  className,
  testId,
}: {
  resolved: ResolvedAmount;
  className?: string;
  testId?: string;
}) {
  const { t } = useTranslation();
  return (
    <span className={cn("inline-flex items-center gap-1", className)} data-testid={testId}>
      {formatMinor(resolved.amountMinor, resolved.currency)}
      {resolved.noRate && (
        <span
          data-testid={testId ? `${testId}-not-converted` : undefined}
          className="inline-flex shrink-0 text-muted-foreground"
          title={t("displayCurrency.notConverted")}
        >
          <Info size={14} />
        </span>
      )}
    </span>
  );
}
