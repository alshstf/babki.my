import { Info } from "lucide-react";
import { useTranslation } from "react-i18next";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
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
//
// When the figure *was* converted, the date of the fx rate behind it
// (`resolved.rateOn`) is disclosed in the cell's own `title` tooltip — never
// as text beside the number, which is visual noise for a detail nobody reads
// at a glance (same rule as the quote date in positions-table.tsx and the
// stale-rates icon in summary-cards.tsx). The two states are mutually
// exclusive by construction: a converted amount carries a rate date and no
// indicator, an unconverted one carries the indicator and no rate date.
export function MoneyCell({
  resolved,
  className,
  testId,
  // Wording for the "couldn't convert this" indicator. Defaults to the
  // account phrasing ("shown in the account's currency"), which is wrong for
  // position rows — their amounts are in the position's, the quote's or a
  // bond face value's currency — so those callers pass their own.
  notConvertedTitle,
  // Wording for the tooltip that discloses the fx rate date behind a
  // converted figure, given that date already formatted. Defaults to the
  // current-rate phrasing, which is right for balances and positions ("what
  // is this worth now"); the operations journal converts at the rate in
  // effect on the operation's own date, and re-using the current-rate wording
  // there would lie about what the number means, so it passes its own.
  //
  // A function rather than a ready string so each caller's t() call keeps a
  // literal key at the call site (scripts/check-i18n.mjs can only verify
  // those) while the date formatting — and the malformed-date rule below —
  // stay owned by this component.
  convertedTitle,
}: {
  resolved: ResolvedAmount;
  className?: string;
  testId?: string;
  notConvertedTitle?: string;
  convertedTitle?: (formattedDate: string) => string;
}) {
  const { t } = useTranslation();
  // A malformed rate date yields no tooltip at all rather than a broken one
  // (the "converted at the rate of ..." wording left dangling with no date),
  // matching how formatDate's empty result is handled everywhere else.
  const rateDate = resolved.rateOn ? formatDate(resolved.rateOn) : "";
  const convertedTooltip = rateDate
    ? (convertedTitle?.(rateDate) ?? t("displayCurrency.convertedOn", { date: rateDate }))
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
          <Info size={14} />
        </span>
      )}
    </span>
  );
}
