import { useTranslation } from "react-i18next";
import { formatMinor, signClass } from "@/lib/money";
import { cn } from "@/lib/utils";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import type { Position } from "@/api/positions";

// Which kind of gap stopped the base-currency total. The two are not the same
// news to the person reading it, and the API publishes a separate flag for
// each precisely so a client never explains one with the other's sentence:
// - "undated": some purchase date was never recorded, so no rate answers for
//   it and no figure will ever appear.
// - "no_rate": the rate for a date that IS known has not been fetched yet; the
//   backfill closes that gap on its own and the figure shows up later.
// - "both": different positions were stopped by different gaps.
export type RealizedGap = "undated" | "no_rate" | "both";

export interface RealizedSum {
  // One entry per currency, ordered by currency code. In "base" mode there is
  // at most one (the base currency); in "native" mode there is one per
  // position currency, because amounts in different currencies are not
  // addable and a single integer sum of them would be denominated in nothing.
  totals: { currency: string; amountMinor: number }[];
  // Non-null only when at least one position's figure could not be resolved —
  // which can only happen in "base" mode. `totals` is then empty: a total
  // missing one of its terms is an invented number, smaller or larger than the
  // truth and indistinguishable from a real one on screen.
  gap: RealizedGap | null;
}

// The realized figure of one position in the mode's currency, or null with the
// reason it is unknown. Mirrors resolveDisplayAmount's mode logic — including
// its first branch: a position already denominated in the base currency
// carries no `in_base` at all (nothing to convert), so its own
// realized_pnl_minor IS the base-currency figure.
function realizedOf(
  position: Position,
  mode: DisplayCurrencyMode,
  baseCurrency: string,
): { amountMinor: number; currency: string; gap: null } | { amountMinor: null; gap: RealizedGap } {
  if (mode === "native" || position.currency === baseCurrency) {
    return { amountMinor: position.realized_pnl_minor, currency: position.currency, gap: null };
  }
  const converted = position.in_base?.realized_pnl_minor;
  if (converted != null) {
    return { amountMinor: converted, currency: baseCurrency, gap: null };
  }
  // Which flag explains this null depends on WHERE the figure went missing.
  // `has_undated_realizations` is the one that speaks about this figure: it
  // says a parcel of basis already sold does not know when it was bought.
  // `has_undated_lots` speaks about the lots still HELD and cannot explain a
  // realized figure by itself — but when the whole `in_base` object is absent,
  // an undated held lot is what nulled the object this figure lives in, so it
  // is the honest answer to "why is there no number", and it is the same
  // permanent kind of gap. Reading it in the other case (object present,
  // realized figure null) would name a cause that is not the cause.
  const undated =
    position.has_undated_realizations || (position.in_base == null && position.has_undated_lots);
  return { amountMinor: null, gap: undated ? "undated" : "no_rate" };
}

// Adds up what the account's positions have actually locked in.
//
// This is the one place in the frontend that adds money, and it is deliberate.
// The project's rule — the frontend does no money arithmetic — exists to keep
// conversion and rounding on the server, where the rates and the decimal math
// live: every term added here was already converted and already rounded by the
// backend (see PositionInBase.realized_pnl_minor). What is left is exact
// integer addition of int64 minor units that are all in ONE currency, applies
// no rate, rounds nothing, and cannot lose a fraction — the same kind of
// display-only derivation as the profit percentage in positions-table.tsx.
// The two conditions that make it safe are enforced here rather than assumed:
// amounts are grouped by currency and never mixed, and a total whose terms are
// not all known is not published at all.
export function sumRealized(
  positions: readonly Position[],
  mode: DisplayCurrencyMode,
  // The space's base currency — needed to tell "already in base, nothing to
  // convert" apart from "conversion failed", exactly as resolveDisplayAmount
  // needs it.
  baseCurrency: string,
): RealizedSum {
  const byCurrency = new Map<string, number>();
  let undated = false;
  let noRate = false;
  for (const position of positions) {
    const resolved = realizedOf(position, mode, baseCurrency);
    if (resolved.amountMinor === null) {
      if (resolved.gap === "undated") undated = true;
      else noRate = true;
      continue;
    }
    byCurrency.set(
      resolved.currency,
      (byCurrency.get(resolved.currency) ?? 0) + resolved.amountMinor,
    );
  }
  if (undated || noRate) {
    // Every gap is a base-mode gap (see realizedOf), so everything gathered
    // above is in the base currency and dropping it drops exactly the partial
    // total that must not be shown.
    return { totals: [], gap: undated && noRate ? "both" : undated ? "undated" : "no_rate" };
  }
  return {
    totals: [...byCurrency.entries()]
      .map(([currency, amountMinor]) => ({ currency, amountMinor }))
      .sort((a, b) => a.currency.localeCompare(b.currency)),
    gap: null,
  };
}

// The wording for a gap: what is shown instead of the sum, and the tooltip
// that says why no partial sum is shown in its place. Written as a switch over
// literal keys rather than a lookup table, so every key stays a literal at the
// t() call site — that is the only shape scripts/check-i18n.mjs can verify.
function gapWording(
  t: (key: string) => string,
  gap: RealizedGap,
): { text: string; hint: string } {
  switch (gap) {
    case "undated":
      return {
        text: t("positions.realizedGapUndated"),
        hint: t("positions.realizedGapUndatedHint"),
      };
    case "no_rate":
      return {
        text: t("positions.realizedGapNoRate"),
        hint: t("positions.realizedGapNoRateHint"),
      };
    case "both":
      return {
        text: t("positions.realizedGapBoth"),
        hint: t("positions.realizedGapBothHint"),
      };
  }
}

// The account header's "Зафиксировано" line: what the closed deals have
// actually locked in, across every position of the account.
//
// Deliberately NOT a column in the positions table — the owner removed the
// per-row "Реализовано" as visual noise, and that decision stands; one figure
// in the header answers the question without putting it back into every row.
//
// What separates it from the profit in the table is that both of its ends are
// past events with dates of their own: it will never move again, and in the
// base currency it carries the currency's own move between purchase and sale.
// That is exactly the kind of mechanics the owner wants disclosed on demand
// rather than printed as text, so it lives in the label's tooltip.
export function RealizedTotal({
  positions,
  mode,
  baseCurrency,
}: {
  positions: Position[];
  mode: DisplayCurrencyMode;
  baseCurrency: string;
}) {
  const { t } = useTranslation();
  const { totals, gap } = sumRealized(positions, mode, baseCurrency);
  // An account with no positions has nothing to say here — a "0,00" over an
  // empty account is an answer to a question nobody asked. A real zero across
  // real positions is shown: it is a fact, and hiding it would be the silence
  // this screen is not allowed to keep.
  if (!gap && totals.length === 0) return null;
  const wording = gap ? gapWording(t, gap) : null;
  return (
    <div className="mt-2 flex flex-wrap items-baseline gap-x-2 text-sm" data-testid="realized-total">
      <span
        data-testid="realized-total-label"
        className="text-muted-foreground"
        title={t("positions.realizedHint")}
      >
        {t("positions.realizedTitle")}
      </span>
      {wording ? (
        <span
          data-testid="realized-total-gap"
          className="text-muted-foreground"
          title={wording.hint}
        >
          {wording.text}
        </span>
      ) : (
        <span data-testid="realized-total-amounts" className="font-medium tabular-nums">
          {totals.map((total, index) => (
            <span key={total.currency}>
              {index > 0 && <span className="text-muted-foreground"> · </span>}
              <span className={cn(signClass(total.amountMinor))}>
                {formatMinor(total.amountMinor, total.currency)}
              </span>
            </span>
          ))}
        </span>
      )}
    </div>
  );
}
