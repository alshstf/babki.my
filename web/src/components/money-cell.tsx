import { Info, Scale } from "lucide-react";
import { useTranslation } from "react-i18next";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { cn } from "@/lib/utils";
import type { ResolvedAmount } from "@/lib/display-amount";

// MoneyCell renders one resolved amount (see resolveDisplayAmount): the number
// and, when `resolved.noRate`, an indicator that it is shown in its native
// currency; why is the caller's sentence (notConvertedTitle), since only the
// server knows the cause. When converted, the rate date goes in the cell's
// tooltip. caveatTitle is a separate indicator about what the number is (a cost
// basis by another country's rules), a different statement.
export function MoneyCell({
  resolved,
  className,
  testId,
  // The "could not convert" wording; the default names the account's
  // currency, so position rows pass their own.
  notConvertedTitle,
  // The tooltip disclosing a converted figure's rate date, given the date
  // formatted. The default says "current rate", right for balances and market
  // values; the journal and position cost/income pass their own. A function so
  // callers keep literal t() keys. It receives null when there is no usable date;
  // a wording that needs one returns undefined and the tooltip is withheld.
  convertedTitle,
  // A caveat about what the figure is, its own indicator, independent of
  // conversion (the journal's cost-basis caveat). Absent by default.
  caveatTitle,
}: {
  resolved: ResolvedAmount;
  className?: string;
  testId?: string;
  notConvertedTitle?: string;
  convertedTitle?: (formattedDate: string | null) => string | undefined;
  caveatTitle?: string;
}) {
  const { t } = useTranslation();
  // Disclosure follows `converted`, not the date: cost and income are converted
  // at many dates and carry none. The default wording needs a date and is
  // skipped without one; a caller's own wording decides for itself.
  const rateDate = resolved.rateOn ? formatDate(resolved.rateOn) : "";
  const convertedTooltip = !resolved.converted
    ? undefined
    : convertedTitle
      ? convertedTitle(rateDate || null)
      : rateDate
        ? t("displayCurrency.convertedOn", { date: rateDate })
        : undefined;
  return (
    <span
      className={cn("inline-flex items-center gap-1", className)}
      data-testid={testId}
      title={convertedTooltip}
    >
      {formatMinor(resolved.amountMinor, resolved.currency)}
      {resolved.noRate && (
        <span
          data-testid={testId ? `${testId}-not-converted` : undefined}
          className="inline-flex shrink-0 text-muted-foreground"
          title={notConvertedTitle ?? t("displayCurrency.notConverted")}
        >
          <Info size={14} aria-hidden="true" />
          {/* The sentence again for screen readers (#31): a title on a
             non-focusable span is not announced, and the number would read as
             ordinary. title stays for the pointer. */}
          <span className="sr-only">{notConvertedTitle ?? t("displayCurrency.notConverted")}</span>
        </span>
      )}
      {caveatTitle && (
        // A different glyph: both indicators can sit on one figure.
        <span
          data-testid={testId ? `${testId}-caveat` : undefined}
          className="inline-flex shrink-0 text-muted-foreground"
          title={caveatTitle}
        >
          <Scale size={14} aria-hidden="true" />
          <span className="sr-only">{caveatTitle}</span>
        </span>
      )}
    </span>
  );
}
