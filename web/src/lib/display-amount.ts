// Picks which of two server-supplied numbers to show for one money value: the
// native amount or the converted base-currency one, by display mode. No money
// arithmetic happens here; reading a figure's currency off the payload is not
// arithmetic.
import type { DisplayCurrencyMode } from "./display-currency";

/**
 * One converted money figure as the server publishes it: amount, its currency
 * and the fx rate date. They travel together because the currency used to come
 * from the session's base_currency, which changes at once when the base currency
 * is edited while cached payloads still hold the old conversion (#106): the wrong
 * sign on a number wrong by the whole rate. `currency` is required, as on every
 * in-base object in the contract.
 */
export interface ConvertedFigure {
  /**
   * The amount in `currency`'s minor units, or null/undefined when the block
   * exists but this term does not (a nullable market_value_minor).
   */
  amountMinor: number | null | undefined;
  /** The currency `amountMinor` is in, as published beside it. */
  currency: string;
  /**
   * The fx rate date behind `amountMinor` when there is a single one; a
   * position's cost has one per purchase day and names none.
   */
  rateOn?: string | null;
}

export interface ResolvedAmount {
  amountMinor: number;
  currency: string;
  /**
   * True when mode is "base" and no converted figure was published, so the
   * native amount is shown flagged rather than a dash or a zero. Why it is missing
   * is not this flag's to say: a rate is one cause, an unrecorded purchase date
   * another, and only the server knows (in_base_gap). The two tables pass their own
   * wording; only the accounts screens use displayCurrency.notConverted, which
   * names a rate, the only cause for a balance.
   */
  noRate: boolean;
  /**
   * True when `amountMinor` is the server's converted figure. False when the
   * mode asks for native, the native currency is the base one, or conversion was
   * unavailable. Not derivable from `rateOn`: a converted cost or income has no
   * single rate date.
   */
  converted: boolean;
  /**
   * The fx rate date (in_base.rate_on, balance_in_base.rate_on) when the
   * converted figure is shown, so MoneyCell can disclose it; null when the native
   * amount is shown or the figure has no single rate date.
   */
  rateOn: string | null;
}

// - "native" mode, or the native currency already equals the base currency
//   (nothing to convert — the backend never populates a base figure in this
//   case either, so this check must come first): show the native amount.
// - "base" mode with a converted figure available: show it, in the currency
//   THAT FIGURE says it is in.
// - "base" mode with no converted figure (the server published none, for a
//   reason this function is not told — see ResolvedAmount.noRate): show
//   the native amount, flagged `noRate`.
export function resolveDisplayAmount(
  mode: DisplayCurrencyMode,
  nativeCurrency: string,
  nativeAmountMinor: number,
  // The space's base currency, used only to answer whether there was anything
  // to convert when no converted figure exists; the currency shown never comes
  // from it.
  baseCurrency: string,
  // The server's converted figure for this cell, or null/undefined when it
  // published none.
  converted?: ConvertedFigure | null,
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
  if (converted != null && converted.amountMinor != null) {
    return {
      amountMinor: converted.amountMinor,
      currency: converted.currency,
      noRate: false,
      converted: true,
      rateOn: converted.rateOn ?? null,
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

/**
 * resolveDisplayAmount for a figure that may be missing in either currency
 * independently (realized, settled, total): a disposal settled in a third
 * currency has no native figure but a base one; missing rates the reverse.
 * Returns null only when the chosen mode's figure is missing (a dash); in base
 * mode a missing converted figure still falls back to the native one.
 */
export function resolveOptionalDisplayAmount(
  mode: DisplayCurrencyMode,
  nativeCurrency: string,
  nativeAmountMinor: number | null | undefined,
  baseCurrency: string,
  converted?: ConvertedFigure | null,
): ResolvedAmount | null {
  if (mode === "native" || nativeCurrency === baseCurrency) {
    if (nativeAmountMinor == null) return null;
    return resolveDisplayAmount(mode, nativeCurrency, nativeAmountMinor, baseCurrency, converted);
  }
  if (converted != null && converted.amountMinor != null) {
    return {
      amountMinor: converted.amountMinor,
      currency: converted.currency,
      noRate: false,
      converted: true,
      rateOn: converted.rateOn ?? null,
    };
  }
  if (nativeAmountMinor == null) return null;
  return resolveDisplayAmount(mode, nativeCurrency, nativeAmountMinor, baseCurrency, converted);
}
