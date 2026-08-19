import { useTranslation } from "react-i18next";
import { formatMinor, signClass } from "@/lib/money";
import { cn } from "@/lib/utils";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import type {
  RealizedGap,
  RealizedTotal as RealizedTotalPayload,
} from "@/api/positions";

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

// The account header's "Реализованная прибыль" line: what the closed deals have
// actually locked in, across every position of the account.
//
// It adds nothing. The server publishes both forms of the total — one figure
// per position currency, and one in the space's base currency — and this
// component picks the one the display-currency toggle asks for. Money
// arithmetic lives on the server even when it would be exact here, so that the
// figure has a single definition and the rounding and gap policies behind it
// can change in one place (see RealizedTotal in the API contract).
//
// It is the ACCOUNT's figure, and the reason it is not the same word as the
// table's "Зафиксировано" column is that it is not the same quantity: this is
// closed deals alone, while that column adds the payments the paper made. The
// row below does show its own realized result, as a second line under the
// profit rather than a column of its own — the owner removed a "Реализовано"
// column once as visual noise, and that decision is what keeps it off the
// header row here.
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

  // In the base currency the response carries either the figure or the reason
  // there is none, never both.
  //
  // IN THE POSITIONS' OWN CURRENCY A BUCKET CAN BE NULL TOO, and it is not a
  // gap of the same kind: nothing is missing and no rate would help — one of
  // the positions in it sold into another currency, so the bucket has no total
  // in ONE currency to state (see Position.realized_pnl_minor in the contract).
  // Such a bucket is dropped from the line rather than drawn as a zero, which
  // is the one rendering that would be a lie: nought is an ordinary realized
  // result and the two would be indistinguishable. If that leaves nothing at
  // all, the line disappears exactly as it does for an account with no
  // positions, and the base-currency mode is where that money is still a
  // figure.
  // AN ACCOUNT WITH NO CLOSED DEALS AT ALL SAYS NOTHING ABOUT THEM — not even
  // the base currency's nought, which the server does publish (the sum of no
  // deals is 0, honestly) and which would otherwise print as a realized result
  // over an account that has never sold anything. by_currency is empty exactly
  // in that case, and it is the one field that distinguishes it: a real zero
  // across real positions has a bucket and IS shown, because nought is a fact
  // and hiding it would be the silence this screen is not allowed to keep.
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
  // THE TAX THE ACCOUNT ITSELF WAS CHARGED, beside the result it was charged
  // against. It is not part of any position's figures and never can be — the
  // broker takes it against the year's accumulated base, not against a paper —
  // so it stands as its own line rather than being folded into the total above.
  //
  // Shown in EVERY mode, unconverted, because that is what it is: money taken
  // in the currency it was taken in. The realized total beside it converts;
  // this one has no per-payment dates to convert by and the contract says so.
  const tax = total.tax_withheld_by_currency.filter(
    (entry) => entry.amount_minor !== 0,
  );

  // NOTHING TO SAY, SAID BY SAYING NOTHING. Three ways this line can be empty
  // and all three are checked together: no figure, no wording for the gap that
  // stopped one, and no tax withheld. An account with no positions at all
  // arrives here with all three empty — a "0,00" over an empty account is an
  // answer to a question nobody asked — while a real zero across real positions
  // IS a figure and IS shown, because nought is a fact and hiding it would be
  // the silence this screen is not allowed to keep.
  //
  // The tax is checked BESIDE the figures rather than after them, which is the
  // whole reason this is one guard: the two are independent. A broker that
  // records the tax on a dividend as its own operation with no paper attached
  // gives an account a withholding with no realized result anywhere — and an
  // earlier version of this component returned before it ever looked, so that
  // money was on the screen nowhere at all.
  //
  // Checked against wording rather than gap itself — a gap can be non-null yet
  // unnameable (see assertUnreachable), and a reason invented here is exactly
  // what the named gap exists to prevent, so that case must fall through to the
  // same blank as no gap at all rather than render the label over an empty
  // amount.
  if (!wording && figures.length === 0 && tax.length === 0) return null;
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
    </div>
  );
}
