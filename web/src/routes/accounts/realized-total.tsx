import { useTranslation } from "react-i18next";
import { formatMinor, signClass } from "@/lib/money";
import { cn } from "@/lib/utils";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import type { RealizedGap, RealizedTotal as RealizedTotalPayload } from "@/api/positions";

// assertUnreachable is gapWording's runtime backstop for a RealizedGap value
// this build cannot name. TypeScript proves the switch below exhaustive over
// the union it knows about — a NEW member added to the contract's enum
// without a case here fails to compile, because `gap` at the default branch
// would no longer narrow to `never`. But `gap` is JSON off the wire, typed by
// assertion rather than validated, so a client running slightly behind the
// server it talks to can still receive a literal outside the union it was
// built with; that must degrade at runtime, not throw or fabricate a label.
function assertUnreachable(_: never): undefined {
  return undefined;
}

// The wording for a gap: what is shown instead of the sum, and the tooltip that
// says why no partial sum is shown in its place. The gap itself arrives named
// from the server (RealizedTotal.in_base_gap) — this screen never works it out
// from the positions' flags, because the two kinds are not the same news to the
// reader and only the server knows which one actually stopped the sum.
//
// Written as a switch over literal keys rather than a lookup table, so every
// key stays a literal at the t() call site — that is the only shape
// scripts/check-i18n.mjs can verify.
//
// Returns undefined for a gap value outside the three named above (see
// assertUnreachable) — the caller must then show nothing rather than the
// label over an empty amount, which is what happens if the label's row
// renders as soon as ANY gap is non-null without checking that wording for
// it actually exists.
function gapWording(
  t: (key: string) => string,
  gap: RealizedGap,
): { text: string; hint: string } | undefined {
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
    default:
      return assertUnreachable(gap);
  }
}

// The account header's "Зафиксировано" line: what the closed deals have
// actually locked in, across every position of the account.
//
// It adds nothing. The server publishes both forms of the total — one figure
// per position currency, and one in the space's base currency — and this
// component picks the one the display-currency toggle asks for. Money
// arithmetic lives on the server even when it would be exact here, so that the
// figure has a single definition and the rounding and gap policies behind it
// can change in one place (see RealizedTotal in the API contract).
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
  total,
  mode,
}: {
  total: RealizedTotalPayload;
  mode: DisplayCurrencyMode;
}) {
  const { t } = useTranslation();
  // An account with no positions has nothing to say here — a "0,00" over an
  // empty account is an answer to a question nobody asked, and by_currency is
  // empty exactly then. A real zero across real positions IS shown: it is a
  // fact, and hiding it would be the silence this screen is not allowed to
  // keep.
  if (total.by_currency.length === 0) return null;

  // In the positions' own currency every figure is published unconditionally,
  // so that mode never has a gap. In the base currency the response carries
  // either the figure or the reason there is none, never both.
  const gap = mode === "base" ? total.in_base_gap : null;
  const figures =
    mode === "base"
      ? total.in_base == null
        ? []
        : [{ currency: total.base_currency, amountMinor: total.in_base }]
      : total.by_currency.map((entry) => ({
          currency: entry.currency,
          amountMinor: entry.realized_pnl_minor,
        }));

  const wording = gap ? gapWording(t, gap) : undefined;
  // Neither a figure nor a wording for the gap that stopped it: nothing this
  // screen could say would be true. Checked against wording rather than gap
  // itself — a gap can be non-null yet unnameable (see assertUnreachable),
  // and a reason invented here is exactly what the named gap exists to
  // prevent, so that case must fall through to the same blank as no gap at
  // all rather than render the label over an empty amount.
  if (!wording && figures.length === 0) return null;
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
    </div>
  );
}
