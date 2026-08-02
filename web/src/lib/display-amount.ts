// Decides which of two backend-supplied numbers to show for a single money
// value: the native amount (in the row's own currency) or the pre-converted
// base-currency amount, depending on the user's display-currency mode. This
// never does money arithmetic itself — it only picks between two numbers
// the backend already computed (money.ts and the backend own all conversion
// math), per the project's "frontend does no money arithmetic" rule.
import type { DisplayCurrencyMode } from "./display-currency";

export interface ResolvedAmount {
  amountMinor: number;
  currency: string;
  /**
   * True when mode is "base" but no fx rate was available for this
   * currency, so the native amount is shown as an honest fallback instead
   * — never a dash, never a fabricated zero. Callers should pair this with
   * a small indicator (title: displayCurrency.notConverted).
   */
  noRate: boolean;
  /**
   * True when `amountMinor` is the backend's converted, base-currency figure
   * rather than the row's own. False in every other case, and the three are
   * genuinely different: the mode asks for the native amount, the native
   * currency already IS the base currency (nothing was converted because
   * nothing needed to be), or the conversion was unavailable (`noRate`).
   *
   * It exists because `rateOn` cannot answer this question. A converted
   * figure does not always carry a rate date — a position's in_base publishes
   * `rate_on` only when it holds the market valuation that date belongs to
   * (see PositionInBase.rate_on in the API contract) — while its cost and
   * income are converted all the same, at the rates of their own many dates.
   * A caller keying "was this converted" off `rateOn` therefore drops the
   * disclosure on figures that WERE converted, which is how the two got
   * conflated in the first place.
   */
  converted: boolean;
  /**
   * Date (YYYY-MM-DD) of the fx rate behind `amountMinor`, straight from
   * the backend's in_base.rate_on / balance_in_base.rate_on — non-null
   * only when the converted figure is what's actually being shown, so a
   * caller can disclose how stale that conversion is (MoneyCell puts it in
   * the cell's tooltip). Null whenever the native amount is shown: no
   * conversion happened, so there is no rate date to name — and also when a
   * converted figure has no single rate date to name (see `converted`).
   */
  rateOn: string | null;
}

// - "native" mode, or the native currency already equals the base currency
//   (nothing to convert — the backend never populates a base figure in this
//   case either, so this check must come first): show the native amount.
// - "base" mode with a converted figure available: show it, in the base
//   currency.
// - "base" mode with no converted figure (no fx rate was resolvable): show
//   the native amount, flagged `noRate`.
export function resolveDisplayAmount(
  mode: DisplayCurrencyMode,
  nativeCurrency: string,
  nativeAmountMinor: number,
  baseCurrency: string,
  baseAmountMinor: number | null | undefined,
  // The fx rate date that produced baseAmountMinor (in_base.rate_on /
  // balance_in_base.rate_on). Optional: callers that have no such figure to
  // show can omit it. It is only ever carried into the result alongside the
  // converted amount itself — see ResolvedAmount.rateOn.
  baseRateOn?: string | null,
): ResolvedAmount {
  if (mode === "native" || nativeCurrency === baseCurrency) {
    return {
      amountMinor: nativeAmountMinor,
      currency: nativeCurrency,
      noRate: false,
      converted: false,
      rateOn: null,
    };
  }
  if (baseAmountMinor != null) {
    return {
      amountMinor: baseAmountMinor,
      currency: baseCurrency,
      noRate: false,
      converted: true,
      rateOn: baseRateOn ?? null,
    };
  }
  return {
    amountMinor: nativeAmountMinor,
    currency: nativeCurrency,
    noRate: true,
    converted: false,
    rateOn: null,
  };
}
