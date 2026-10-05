import { useTranslation } from "react-i18next";
import { formatMinor, signClass } from "@/lib/money";
import { cn } from "@/lib/utils";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import type {
  RealizedGap,
  RealizedTotal as RealizedTotalPayload,
} from "@/api/positions";

// assertUnreachable backs gapWording's switch at runtime: TypeScript proves it
// exhaustive, but `gap` is unvalidated JSON, and a newer server's value must
// degrade, not throw.
function assertUnreachable(_: never): undefined {
  return undefined;
}

// The wording for a gap: what stands in for the sum and why no partial sum is
// shown. The gap comes named from the server (RealizedTotal.in_base_gap), never
// worked out from positions. Literal t() keys for scripts/check-i18n.mjs.
// Undefined for an unnamed value, and the caller then shows nothing.
function gapWording(
  t: (key: string) => string,
  gap: RealizedGap,
): { text: string; hint: string } | undefined {
  switch (gap) {
    case "no_rate":
      return {
        text: t("positions.realizedGapNoRate"),
        hint: t("positions.realizedGapNoRateHint"),
      };
    default:
      return assertUnreachable(gap);
  }
}

// The account's realized line: what closed deals have locked in, across every
// position. It adds nothing: the server publishes per-currency and base-currency
// totals and this picks by mode. It is closed deals only, unlike the table's
// «Зафиксировано», which adds payments. Both ends are past events, so it never
// moves and in base currency carries the currency's move; that is explained in
// the label's tooltip.
export function RealizedTotal({
  total,
  mode,
}: {
  total: RealizedTotalPayload;
  mode: DisplayCurrencyMode;
}) {
  const { t } = useTranslation();

  // In base currency the response carries the figure or its gap, never
  // both. In the positions' currencies a bucket can be null too: a position sold
  // into another currency has no single-currency total. It is dropped rather than
  // drawn as zero; if nothing remains, the line disappears and base mode still has
  // the figure.
  //
  // An account with no closed deals says nothing, not even the server's base-currency
  // zero: by_currency is empty exactly then. A real zero across real positions
  // has a bucket and is shown.
  const anyRealized = total.by_currency.length > 0;
  const gap = anyRealized && mode === "base" ? total.in_base_gap : null;
  const figures = !anyRealized
    ? []
    : mode === "base"
      ? total.in_base == null
        ? []
        : [{ currency: total.base_currency, amountMinor: total.in_base }]
      : total.by_currency
          .filter((entry) => entry.realized_pnl_minor != null)
          .map((entry) => ({
            currency: entry.currency,
            amountMinor: entry.realized_pnl_minor as number,
          }));

  const wording = gap ? gapWording(t, gap) : undefined;
  // Positions sold without a recorded purchase day have no base-currency result;
  // the server leaves them out and counts them. Base mode only.
  const undated = anyRealized && mode === "base" ? total.undated_positions : 0;
  // Sales of shares that arrived with no price: the whole proceeds count as
  // profit. True in every currency.
  const unknownCost = anyRealized ? total.unknown_cost_positions : 0;
  // The tax the account was charged, beside the result: taken against the
  // year's base, not a paper, so its own line. Shown unconverted in every mode:
  // it has no per-payment dates to convert by.
  const tax = total.tax_withheld_by_currency.filter(
    (entry) => entry.amount_minor !== 0,
  );

  // Nothing to say: no figure, no wording for a gap, no tax, nothing
  // undated or unpriced. A real zero is shown. Tax is checked alongside the
  // figures, since a withholding can exist with no realized result. Checked
  // against the wording, not the gap, so an unnamed gap shows nothing.
  if (!wording && figures.length === 0 && tax.length === 0 && undated === 0 && unknownCost === 0)
    return null;
  return (
    <div
      className="mt-2 flex flex-wrap items-baseline gap-x-2 text-sm"
      data-testid="realized-total"
    >
      <span
        data-testid="realized-total-label"
        className="text-muted-foreground"
        title={t("positions.realizedHint")}
      >
        {t("positions.realizedTitle")}
      </span>
      {tax.length > 0 && (
        <span
          data-testid="realized-total-tax"
          className="text-muted-foreground"
          title={t("positions.taxWithheldHint")}
        >
          {t("positions.taxWithheld", {
            amounts: tax
              .map((entry) => formatMinor(entry.amount_minor, entry.currency))
              .join(" · "),
          })}
        </span>
      )}
      {wording ? (
        <span
          data-testid="realized-total-gap"
          className="text-muted-foreground"
          title={wording.hint}
        >
          {wording.text}
        </span>
      ) : (
        <span
          data-testid="realized-total-amounts"
          className="font-medium tabular-nums"
        >
          {figures.map((figure, index) => (
            <span key={figure.currency}>
              {index > 0 && <span className="text-muted-foreground"> · </span>}
              <span className={cn(signClass(figure.amountMinor))}>
                {formatMinor(figure.amountMinor, figure.currency)}
              </span>
            </span>
          ))}
        </span>
      )}
      {undated > 0 && (
        <span
          data-testid="realized-total-undated"
          className="text-muted-foreground"
          title={t("positions.realizedUndatedHint")}
        >
          {t("positions.realizedUndated", { count: undated })}
        </span>
      )}
      {unknownCost > 0 && (
        <span
          data-testid="realized-total-unknown-cost"
          className="text-amber-600"
          title={t("positions.realizedUnknownCostHint")}
        >
          {t("positions.realizedUnknownCost", { count: unknownCost })}
        </span>
      )}
    </div>
  );
}
