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
): ResolvedAmount {
  if (mode === "native" || nativeCurrency === baseCurrency) {
    return { amountMinor: nativeAmountMinor, currency: nativeCurrency, noRate: false };
  }
  if (baseAmountMinor != null) {
    return { amountMinor: baseAmountMinor, currency: baseCurrency, noRate: false };
  }
  return { amountMinor: nativeAmountMinor, currency: nativeCurrency, noRate: true };
}
