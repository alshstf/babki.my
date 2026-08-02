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
  // current-rate phrasing, which is right wherever the figure answers "what
  // is this worth now" — account balances, a position's market value. Callers
  // whose figure was converted at some other rate pass their own wording,
  // because re-using the current-rate phrasing would lie about what the
  // number means: the operations journal converts at the rate in effect on
  // the operation's own date, and a position's cost/income/profit are built
  // from the rates on each lot's purchase date and each income operation's
  // date.
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
  // Whether to disclose anything at all is decided by `converted` — did this
  // cell get the backend's base-currency figure or the row's own — and not by
  // whether a rate date came with it. The two are not the same question: a
  // position's cost and income are converted at the rates of their own many
  // dates and carry no single date to name, and keying off the date silently
  // dropped their disclosure whenever the row had nothing else to date (a
  // position with no quote publishes a null rate_on — see PositionInBase in
  // the API contract).
  //
  // The DEFAULT wording is the one that needs the date ("converted at the
  // current rate, on <date>"), so it is skipped when there is none, or when
  // formatDate rejects a malformed one — a dangling "on " reads worse than
  // silence. A caller that supplies its own wording gets it whenever a
  // conversion happened; a wording that interpolates the date is only passed
  // where the contract guarantees one (the journal's every in_base has a
  // rate_on; a position's market value has one exactly when it has a value).
  const rateDate = resolved.rateOn ? formatDate(resolved.rateOn) : "";
  const convertedTooltip = !resolved.converted
    ? undefined
    : convertedTitle
      ? convertedTitle(rateDate)
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
          <Info size={14} />
        </span>
      )}
    </span>
  );
}
