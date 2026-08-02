import { Info, Scale } from "lucide-react";
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
//
// A third, independent thing a cell can carry is a caveat about WHAT the
// number is rather than about which currency it is in (`caveatTitle`). The
// two travel together on the same figure often enough that they need separate
// indicators: "this is shown in dollars because no rate was found" and "this
// is a cost basis your country's rules would have picked differently" are two
// statements, and folding them into one tooltip would be the same conflation
// this component already avoids between "was it converted" and "what date is
// it captioned with".
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
  // those) while the date formatting stays owned by this component.
  //
  // It is handed null — not an empty string — when there is no rate date, or
  // when formatDate rejects a malformed one. A wording that interpolates the
  // date has nowhere to put it and must answer with undefined, which withholds
  // the tooltip: a dangling "— " reads worse than silence. A wording that says
  // nothing about a date ignores the argument and is shown as usual. Which of
  // the two a wording is, only the caller knows, so only the caller can decide
  // — this component used to decide for it by suppressing the tooltip whenever
  // the date was unusable, which silently withheld datelessly-worded
  // disclosures too.
  convertedTitle,
  // A caveat about what this figure IS, shown as its own tooltipped indicator
  // beside the number. Unlike notConvertedTitle it is not about the currency
  // and does not depend on any conversion having happened or failed: the
  // journal uses it to say that a transferred parcel's amount is a cost basis
  // picked by a queue that is not the owner's country's, which is true of the
  // figure whichever currency it ends up displayed in. Absent by default —
  // most cells have nothing of the kind to say.
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
  // silence. A caller that supplies its own wording is handed the same
  // distinction as a null date and answers it itself (see convertedTitle).
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
          <Info size={14} />
        </span>
      )}
      {caveatTitle && (
        // A different glyph from the one above on purpose: both can sit on one
        // figure at once, and two identical icons side by side would read as
        // one thing said twice rather than as two different things said once.
        <span
          data-testid={testId ? `${testId}-caveat` : undefined}
          className="inline-flex shrink-0 text-muted-foreground"
          title={caveatTitle}
        >
          <Scale size={14} />
        </span>
      )}
    </span>
  );
}
