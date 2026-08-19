// Decides which of two backend-supplied numbers to show for a single money
// value: the native amount (in the row's own currency) or the pre-converted
// base-currency amount, depending on the user's display-currency mode. This
// never does money arithmetic itself — it only picks between two numbers
// the backend already computed (money.ts and the backend own all conversion
// math), per the project's "frontend does no money arithmetic" rule. Reading
// which currency a figure is denominated in is not arithmetic either: it is
// reading a field the server published beside the number.
import type { DisplayCurrencyMode } from "./display-currency";

/**
 * One already-converted money figure, exactly as the server publishes it: the
 * amount, THE CURRENCY IT IS DENOMINATED IN, and the date of the fx rate
 * behind it.
 *
 * The three travel together because they belong to one figure, and that shape
 * is the whole of #106. The currency used to be taken from the session's
 * `base_currency` instead — a second source for one answer, and the two come
 * apart in an ordinary way: change the base currency in settings and the
 * session's answer lands at once (useUpdateSpace writes it into the cache
 * directly) while every cached payload still holds figures converted into the
 * OLD one, until its refetch comes back. For that window the ruble figures
 * printed with the new currency's sign — not a mislabelled number but a number
 * wrong by the whole exchange rate, with nothing on screen saying so. A
 * currency that arrives welded to its own amount cannot drift from it.
 *
 * `currency` is required here because the contract makes it required on every
 * object this stands for — MoneyInBase.currency, OperationInBase.currency,
 * PositionInBase.currency — so if the conversion block is there at all, the
 * currency is there with it. It is trusted exactly as far as `amountMinor` is,
 * which is to say as far as the contract goes: both are JSON typed by
 * assertion rather than validated, and neither is second-guessed here.
 */
export interface ConvertedFigure {
  /**
   * The converted amount in `currency`'s minor units, or null/undefined when
   * the server published the conversion block but not this particular term —
   * a position's market_value_minor is nullable inside an otherwise complete
   * PositionInBase.
   */
  amountMinor: number | null | undefined;
  /** The currency `amountMinor` is denominated in, as published beside it. */
  currency: string;
  /**
   * Date (YYYY-MM-DD) of the fx rate that produced `amountMinor`, when the
   * figure has a single one to name. Optional: a position's cost is struck at
   * one rate per purchase day and names none.
   */
  rateOn?: string | null;
}

export interface ResolvedAmount {
  amountMinor: number;
  currency: string;
  /**
   * True when mode is "base" and the server published no converted figure for
   * this cell, so the native amount is shown as an honest fallback instead —
   * never a dash, never a fabricated zero. Callers should pair it with a small
   * indicator.
   *
   * WHY it published none is not this flag's to say, and the name is older
   * than that distinction: a missing fx rate is only one of the causes — a
   * purchase date nobody ever recorded is another, and no rate answers for a
   * date that does not exist — and only the server knows which one it actually
   * stopped on (Position.in_base_gap, Operation.in_base_gap). That is why the
   * two tables pass wordings of their own and only the accounts screens take
   * the default, `displayCurrency.notConverted`, which does name a rate: for an
   * account balance the contract leaves no other cause once there is a balance
   * and the currencies differ.
   */
  noRate: boolean;
  /**
   * True when `amountMinor` is the backend's converted figure rather than the
   * row's own. False in every other case, and the three are genuinely
   * different: the mode asks for the native amount, the native currency
   * already IS the base currency (nothing was converted because nothing needed
   * to be), or the conversion was unavailable (`noRate`).
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
// - "base" mode with a converted figure available: show it, in the currency
//   THAT FIGURE says it is in.
// - "base" mode with no converted figure (the server published none, for a
//   reason this function is not told — see ResolvedAmount.noRate): show
//   the native amount, flagged `noRate`.
export function resolveDisplayAmount(
  mode: DisplayCurrencyMode,
  nativeCurrency: string,
  nativeAmountMinor: number,
  // The space's base currency, from the session or the summary. It answers one
  // question and only one: whether there was anything to convert at all. That
  // question has to be answered when the server published no converted figure,
  // and a figure that does not exist has no currency to read — which is why
  // this argument stays even though the currency SHOWN never comes from it.
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
 * resolveDisplayAmount for a figure that MAY NOT EXIST IN EITHER CURRENCY, and
 * whose two absences are independent.
 *
 * The realized result, the settled result and the total are all like this: a
 * position whose disposal settled in a third currency has no figure in its own
 * currency and a perfectly good one in the base currency, and a position whose
 * fx rates are missing has the reverse. The plain resolver takes a native amount
 * as a required argument and cannot express either case.
 *
 * Returns null only when the figure the CHOSEN MODE would show is missing —
 * which is what the cell renders a dash for. In base mode a missing converted
 * figure still falls back to the native one, exactly as the plain resolver
 * does, so a row keeps saying what it can.
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
