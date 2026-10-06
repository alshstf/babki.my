// Money helpers. Amounts travel as int64 minor units (kopecks/cents);
// conversion to major units happens only here.

const knownCurrency = (currency: string) =>
  ["RUB", "USD", "EUR", "KZT", "GBP", "CHF", "CNY"].includes(currency);

// Writes a number in a currency with the caller's digit options. The one place
// a currency becomes a sign or a code, shared by formatWith (minor units) and
// formatPriceIn (a decimal quote), so both read the same on one screen.
function withCurrency(
  value: number,
  currency: string,
  digits: Intl.NumberFormatOptions,
): string {
  if (knownCurrency(currency)) {
    return new Intl.NumberFormat("ru-RU", { ...digits, style: "currency", currency }).format(value);
  }
  return `${new Intl.NumberFormat("ru-RU", digits).format(value)} ${currency}`;
}

function formatWith(
  amountMinor: number,
  currency: string,
  fractionDigits: number,
): string {
  // -0 would print as "-0,00".
  const normalized = amountMinor === 0 ? 0 : amountMinor;
  return withCurrency(normalized / 100, currency, {
    minimumFractionDigits: fractionDigits,
    maximumFractionDigits: fractionDigits,
  });
}

// Fraction digits of a money amount written in full and in the compact form
// the summary cards use; formatMinorCompact falls back to the full one.
const FULL_FRACTION_DIGITS = 2;
const COMPACT_FRACTION_DIGITS = 0;

// Every amount is kept in hundredths whatever its currency (decision Р-7): a
// currency with no fraction, such as the yen, is written without one when the
// amount is whole; a three-decimal one, such as the Kuwaiti dinar, keeps two.
const wholeCurrencies = new Map<string, boolean>();

function wholeCurrency(currency: string): boolean {
  let whole = wholeCurrencies.get(currency);
  if (whole === undefined) {
    try {
      whole =
        new Intl.NumberFormat("ru-RU", { style: "currency", currency }).resolvedOptions()
          .maximumFractionDigits === 0;
    } catch {
      whole = false;
    }
    wholeCurrencies.set(currency, whole);
  }
  return whole;
}

export function formatMinor(amountMinor: number, currency: string): string {
  const digits =
    amountMinor % 100 === 0 && wholeCurrency(currency) ? 0 : FULL_FRACTION_DIGITS;
  return formatWith(amountMinor, currency, digits);
}

// Whether the compact form would render this amount's magnitude as zero,
// asked of the formatter rather than of a threshold copying Intl's rounding.
// On the magnitude, because a small negative renders as «-0 ₽».
function compactWouldReadAsZero(amountMinor: number, currency: string): boolean {
  return (
    formatWith(Math.abs(amountMinor), currency, COMPACT_FRACTION_DIGITS) ===
    formatWith(0, currency, COMPACT_FRACTION_DIGITS)
  );
}

// The most fraction digits a quantity or price may carry, the backend's
// NUMERIC(30,10). Everything here that promises a derived value the form will
// accept back is written from this one number.
const MAX_FRACTION_DIGITS = 10;

// isPositiveDecimal: a positive decimal with up to MAX_FRACTION_DIGITS
// fraction digits, as the backend validates quantities.
const DECIMAL_RE = new RegExp(`^\\d+(\\.\\d{1,${MAX_FRACTION_DIGITS}})?$`);

export function isPositiveDecimal(value: string): boolean {
  return DECIMAL_RE.test(value) && Number(value) > 0;
}

// formatMinorCompact writes an amount without minor units (1 385 000,00 ₽ as
// «1 385 000 ₽») for the summary cards. An amount that would read as zero is
// written in full instead (#107): forty kopecks are not «0 ₽», and a small debt
// is not «-0 ₽». The full form is the same number, not an approximation.
export function formatMinorCompact(amountMinor: number, currency: string): string {
  if (amountMinor !== 0 && compactWouldReadAsZero(amountMinor, currency)) {
    return formatWith(amountMinor, currency, FULL_FRACTION_DIGITS);
  }
  return formatWith(amountMinor, currency, COMPACT_FRACTION_DIGITS);
}

// A quote below a hundredth but not zero, matched on the digits: the whole
// part zero (leading zeros allowed, as the validator below accepts), the
// fraction starting "00", a non-zero digit somewhere.
const SUB_CENT_PRICE_RE = /^0*0\.00\d*[1-9]/;

// Significant digits shown for a sub-cent price, about the detail two
// fraction digits give an ordinary quote, with no ceiling on scale.
const SUB_CENT_SIGNIFICANT_DIGITS = 3;

// formatPrice renders a decimal-string quote in ru-RU. A hundredth or more gets
// two fraction digits ("305.5" -> "305,50"); below that, significant digits
// ("0.0001" -> "0,0001", "0.000123456" -> "0,000123"), never "0,00" (#30). The
// demo's WeWork at $0.0025 takes that branch. The branch is chosen from the
// string, since a value small enough to underflow would compare as large. Null on
// unparseable input.
export function formatPrice(value: string): string | null {
  const parsed = parsePrice(value);
  return parsed && new Intl.NumberFormat("ru-RU", parsed.digits).format(parsed.num);
}

// The decision formatPrice and formatPriceIn share: is this a price, and how
// many digits are shown. One function, so a sub-cent quote keeps its digits with
// or without a currency. Null for anything not shaped like a price, or a value
// that underflows to zero.
function parsePrice(value: string): { num: number; digits: Intl.NumberFormatOptions } | null {
  if (!/^\d+(\.\d+)?$/.test(value)) return null;
  const num = Number(value);
  if (!Number.isFinite(num)) return null;
  if (SUB_CENT_PRICE_RE.test(value)) {
    // The digits say non-zero and the double says zero: it underflowed. Not
    // reachable from the wire (NUMERIC(30,10)), but "0" is never an answer.
    if (num === 0) return null;
    return { num, digits: { maximumSignificantDigits: SUB_CENT_SIGNIFICANT_DIGITS } };
  }
  return { num, digits: { minimumFractionDigits: 2, maximumFractionDigits: 2 } };
}

// formatPriceIn is formatPrice with the quote's currency named: "305.5" in USD
// as «305,50 $» (#76). In base-currency mode the amount above converts and the
// quote does not, so a bare number would hide that it is dollars. It computes
// nothing: the currency is read off the payload (market_value_source_currency,
// else market_value_currency). Null exactly when formatPrice is.
export function formatPriceIn(value: string, currency: string): string | null {
  const parsed = parsePrice(value);
  return parsed && withCurrency(parsed.num, currency, parsed.digits);
}

// MAX_AMOUNT_MINOR is the largest sum a field sends: 10^15 minor units, the
// server's money.MaxAmountMinor, here so the field refuses at the keystroke. It is
// also below Number.MAX_SAFE_INTEGER, so every value the parser returns is
// exact.
export const MAX_AMOUNT_MINOR = 1_000_000_000_000_000;

// Why an amount field cannot send what was typed: "malformed" (not a number
// of the field's shape) or "tooLarge" (past MAX_AMOUNT_MINOR). Two different
// sentences; callers pick the wording.
export type AmountRefusal = "malformed" | "tooLarge";

// parseAmount is the one parser behind both exports below, so the number and
// the reason cannot disagree.
function parseAmount(input: string): { minor: number } | { refusal: AmountRefusal } {
  const cleaned = input.replace(/\s/g, "").replace(",", "."); // \s matches NBSP (U+00A0) too
  if (!/^-?\d+(\.\d{1,2})?$/.test(cleaned)) {
    return { refusal: "malformed" };
  }
  const [whole, frac = ""] = cleaned.split(".");
  const fracPadded = (frac + "00").slice(0, 2);
  const sign = whole.startsWith("-") ? -1 : 1;
  const wholeAbs = whole.replace("-", "");
  // Magnitude first, avoiding IEEE -0; exact below MAX_SAFE_INTEGER, which
  // the bound below keeps.
  const magnitude = Number(wholeAbs) * 100 + Number(fracPadded);
  // On the magnitude, so a debt is bounded like an asset; a huge whole part
  // is Infinity and fails too.
  if (magnitude > MAX_AMOUNT_MINOR) {
    return { refusal: "tooLarge" };
  }
  return { minor: magnitude === 0 ? 0 : sign * magnitude };
}

// minorToInput is an amount's size as an amount field takes it back
// («2900», «123.45»); the dialog applies the sign.
export function minorToInput(amountMinor: number): string {
  const digits = String(amountMinor).replace("-", "").padStart(3, "0");
  const whole = digits.slice(0, -2);
  const fraction = digits.slice(-2);
  return fraction === "00" ? whole : `${whole}.${fraction}`;
}

// parseToMinor accepts "1 234,56", "1234.56", "-92 000"; null on junk and past
// MAX_AMOUNT_MINOR (amountRefusal says which).
export function parseToMinor(input: string): number | null {
  const parsed = parseAmount(input);
  return "minor" in parsed ? parsed.minor : null;
}

// amountRefusal says why an amount cannot be sent, or null; from the same
// parse as parseToMinor.
export function amountRefusal(input: string): AmountRefusal | null {
  const parsed = parseAmount(input);
  return "refusal" in parsed ? parsed.refusal : null;
}

export function signClass(amountMinor: number): string {
  if (amountMinor > 0) return "text-emerald-500";
  if (amountMinor < 0) return "text-red-500";
  return "text-muted-foreground";
}

// Parses a non-negative plain decimal ("10", "305.5", "0.001") into an exact
// mantissa and digit count; no sign, exponent or separators. Unbounded digits so
// multiplyToMinor stays reusable; fields validate with a stricter regex first.
function parseDecimalString(input: string): { mantissa: bigint; decimals: number } | null {
  if (!/^\d+(\.\d+)?$/.test(input)) return null;
  const [wholePart, fracPart = ""] = input.split(".");
  return { mantissa: BigInt(wholePart + fracPart), decimals: fracPart.length };
}

// Minimum fraction digits of a derived price or percentage ("980.00",
// "98.00"); a minimum only, since renderDecimal keeps every digit the exact
// value has.
const MIN_DERIVED_FRACTION_DIGITS = 2;

// renderDecimal writes an exact mantissa with `decimals` places as a plain
// decimal, trailing zeros trimmed to MIN_DERIVED_FRACTION_DIGITS. A "." separator:
// it is an input value for the wire, not a rendering.
function renderDecimal(mantissa: bigint, decimals: number): string {
  const digits = mantissa.toString().padStart(decimals + 1, "0");
  const whole = digits.slice(0, digits.length - decimals);
  let frac = decimals === 0 ? "" : digits.slice(digits.length - decimals);
  frac = frac.replace(/0+$/, "");
  return `${whole}.${frac.padEnd(MIN_DERIVED_FRACTION_DIGITS, "0")}`;
}

// Fraction digits of a rendered decimal, counted on the text, since
// renderDecimal trims zeros ("980.00" from a ten-place computation).
function fractionDigitsOf(value: string): number {
  const point = value.indexOf(".");
  return point < 0 ? 0 : value.length - point - 1;
}

// bondPriceFromPercent turns a bond quote in percent of face ("98") into the
// money one bond costs in the face currency: 98 % of a 1 000,00 ₽ face is
// "980.00" (#77). Entered as money per unit, 98 would record a cost basis ten
// times too small.
//
// It is the first two factors of the server's marketValue (faceValueMinor ×
// price/100), exact: the product of minor units and a finite decimal is finite and
// written in full. Only multiplyToMinor's total rounds, half away from zero like
// money.Minor (#94): 98,0005 % of a 1 000,00 ₽ face is 98 001 kopecks on both
// sides.
//
// Null when there is nothing to publish: a malformed or zero percentage, a face
// that is absent, zero or negative, or an exact price too fine to store (see the
// end).
export function bondPriceFromPercent(percentOfFace: string, faceValueMinor: number): string | null {
  const percent = parseDecimalString(percentOfFace);
  if (!percent || percent.mantissa === 0n) return null;
  if (!Number.isSafeInteger(faceValueMinor) || faceValueMinor <= 0) return null;
  // Two different divisions by a hundred, minor to major and percent to
  // fraction, kept apart: folding them is the error this function prevents.
  const price = renderDecimal(BigInt(faceValueMinor) * percent.mantissa, percent.decimals + 2 + 2);
  // A percentage fine enough gives an exact price with more digits than a
  // price is stored with, which the form would refuse; refused here instead of
  // rounded, leaving the typed percentage visible.
  return fractionDigitsOf(price) > MAX_FRACTION_DIGITS ? null : price;
}

// Fraction digits the derived percentage is computed to: the stored price
// scale, so its width always fits what the field accepts. A price small enough
// can still round to "0.00", which the field would refuse; acceptable, since this
// value is a display-only caption, never submitted.
const PERCENT_FRACTION_DIGITS = MAX_FRACTION_DIGITS;

// bondPercentFromPrice reads the conversion backwards: 980 ₽ against a 1 000,00 ₽
// face is "98.00". Money to percent need not terminate (100 ₽ against a 3,00 ₽
// face is 3333,333… %), so the last digit is rounded half up at
// PERCENT_FRACTION_DIGITS; a percentage is a caption, and the money price is what
// is sent. Computed as (2n + d) / 2d in integers.
export function bondPercentFromPrice(pricePerUnit: string, faceValueMinor: number): string | null {
  const price = parseDecimalString(pricePerUnit);
  if (!price) return null;
  if (!Number.isSafeInteger(faceValueMinor) || faceValueMinor <= 0) return null;
  // percent = price × 10000 / faceValueMinor.
  const numerator = price.mantissa * 10_000n * 10n ** BigInt(PERCENT_FRACTION_DIGITS);
  const denominator = BigInt(faceValueMinor) * 10n ** BigInt(price.decimals);
  return renderDecimal((2n * numerator + denominator) / (2n * denominator), PERCENT_FRACTION_DIGITS);
}

// multiplyToMinor computes qty × price in integer minor units with BigInt only;
// both operands are non-negative decimal strings and the caller applies the sign.
//
// The reduction to two decimals rounds half away from zero, the server's
// money.Minor rule (#94), so a trade's cost and the server's valuation agree to the
// kopeck. For a non-negative magnitude negated afterwards that is half away from
// zero on the signed figure. (2n + d) / 2d keeps the halving in integers.
//
// Null on malformed input or past Number.MAX_SAFE_INTEGER, checked after rounding
// on the figure returned.
export function multiplyToMinor(qty: string, price: string): number | null {
  const q = parseDecimalString(qty);
  const p = parseDecimalString(price);
  if (!q || !p) return null;

  const productMantissa = q.mantissa * p.mantissa;
  const totalDecimals = q.decimals + p.decimals;
  const MINOR_DECIMALS = 2;

  let minorBig: bigint;
  if (totalDecimals === MINOR_DECIMALS) {
    minorBig = productMantissa;
  } else if (totalDecimals < MINOR_DECIMALS) {
    minorBig = productMantissa * 10n ** BigInt(MINOR_DECIMALS - totalDecimals);
  } else {
    // floor(n/d + 1/2): BigInt division of a non-negative numerator is a
    // floor.
    const divisor = 10n ** BigInt(totalDecimals - MINOR_DECIMALS);
    minorBig = (2n * productMantissa + divisor) / (2n * divisor);
  }

  if (minorBig > BigInt(Number.MAX_SAFE_INTEGER)) return null;
  const result = Number(minorBig);
  return Number.isSafeInteger(result) ? result : null;
}
