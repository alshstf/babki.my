// Money helpers. Amounts travel as int64 minor units (kopecks/cents);
// conversion to major units happens only here.

const knownCurrency = (currency: string) =>
  ["RUB", "USD", "EUR", "KZT", "GBP", "CHF", "CNY"].includes(currency);

function formatWith(
  amountMinor: number,
  currency: string,
  fractionDigits: number,
): string {
  // Normalize -0 to +0 to prevent "-0,00" formatting
  const normalized = amountMinor === 0 ? 0 : amountMinor;
  const major = normalized / 100;
  if (knownCurrency(currency)) {
    return new Intl.NumberFormat("ru-RU", {
      style: "currency",
      currency,
      minimumFractionDigits: fractionDigits,
      maximumFractionDigits: fractionDigits,
    }).format(major);
  }
  const num = new Intl.NumberFormat("ru-RU", {
    minimumFractionDigits: fractionDigits,
    maximumFractionDigits: fractionDigits,
  }).format(major);
  return `${num} ${currency}`;
}

export function formatMinor(amountMinor: number, currency: string): string {
  return formatWith(amountMinor, currency, 2);
}

// isPositiveDecimal validates a positive decimal string with up to 10 fraction
// digits (matches backend NUMERIC(30,10) validation for quantity fields).
const DECIMAL_RE = /^\d+(\.\d{1,10})?$/;

export function isPositiveDecimal(value: string): boolean {
  return DECIMAL_RE.test(value) && Number(value) > 0;
}

export function formatMinorCompact(amountMinor: number, currency: string): string {
  return formatWith(amountMinor, currency, 0);
}

// A quote below a hundredth, but not zero: whole part 0, fraction starting
// "00", and a non-zero digit somewhere in it. Matched on the DIGITS rather
// than on the parsed double on purpose — see formatPrice.
const SUB_CENT_PRICE_RE = /^0\.00\d*[1-9]/;

// How many digits of a sub-cent price are worth showing. Three is the same
// order of detail two fraction digits give an ordinary quote (95,20 is four
// significant digits, 0,05 is one), enough to tell 0,000123 from 0,000456 —
// and unlike a fixed fraction-digit count it needs no ceiling: the price
// picks its own scale.
const SUB_CENT_SIGNIFICANT_DIGITS = 3;

// formatPrice renders a raw decimal-string price (a quote, not minor units)
// as a ru-RU number. A price of a hundredth or more gets exactly two fraction
// digits, e.g. "305.5" -> "305,50" — every quote this program has met so far
// (ruble and dollar shares and bonds).
//
// Below a hundredth those two digits would print "0,00" (#30): a number that
// is neither the price nor zero, one cell away from the column where this
// program refuses to publish a figure it cannot vouch for. So a sub-cent
// price is rendered by significant digits instead — "0.0001" -> "0,0001",
// "0.000123456" -> "0,000123" — and never as a zero. Nothing is quoted that
// finely today; crypto is where it starts.
//
// The branch is chosen from the input STRING, not from the parsed double, for
// the same reason the threshold exists at all: a decimal string small enough
// to underflow to exactly 0 would compare as "not below a hundredth" and take
// the ordinary branch, printing the very "0,00" this avoids.
//
// Returns null on unparseable input so callers can skip the hint entirely
// rather than render garbage — an honest omission over a fake display.
export function formatPrice(value: string): string | null {
  if (!/^\d+(\.\d+)?$/.test(value)) return null;
  const num = Number(value);
  if (!Number.isFinite(num)) return null;
  if (SUB_CENT_PRICE_RE.test(value)) {
    // The digits said non-zero and the double says zero, so the value
    // underflowed and there are no significant digits left to show. Out of
    // reach from the wire — quotes are NUMERIC(30,10), whose smallest
    // non-zero is 1e-10 — but this function's contract is the regex above,
    // and "0" is not an answer it is allowed to give.
    if (num === 0) return null;
    return new Intl.NumberFormat("ru-RU", {
      maximumSignificantDigits: SUB_CENT_SIGNIFICANT_DIGITS,
    }).format(num);
  }
  return new Intl.NumberFormat("ru-RU", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(num);
}

// parseToMinor accepts "1 234,56" / "1234.56" / "-92 000"; returns null on junk.
export function parseToMinor(input: string): number | null {
  const cleaned = input.replace(/\s/g, "").replace(",", "."); // \s matches NBSP (U+00A0) too
  if (!/^-?\d+(\.\d{1,2})?$/.test(cleaned)) {
    return null;
  }
  const [whole, frac = ""] = cleaned.split(".");
  const fracPadded = (frac + "00").slice(0, 2);
  const sign = whole.startsWith("-") ? -1 : 1;
  const wholeAbs = whole.replace("-", "");
  // Compute magnitude first to avoid IEEE -0; integer exactness holds below Number.MAX_SAFE_INTEGER
  const magnitude = Number(wholeAbs) * 100 + Number(fracPadded);
  return magnitude === 0 ? 0 : sign * magnitude;
}

export function signClass(amountMinor: number): string {
  if (amountMinor > 0) return "text-emerald-500";
  if (amountMinor < 0) return "text-red-500";
  return "text-muted-foreground";
}

// Parses a non-negative plain decimal string ("10", "305.5", "0.001") into an
// exact integer mantissa plus its decimal-digit count. No sign, no exponent,
// no thousands separators — trade quantity/price fields are validated with
// their own stricter regex before reaching here; this parser is intentionally
// a bit more permissive (unbounded decimal digits) so multiplyToMinor stays
// reusable. Returns null for anything that isn't a bare non-negative decimal.
function parseDecimalString(input: string): { mantissa: bigint; decimals: number } | null {
  if (!/^\d+(\.\d+)?$/.test(input)) return null;
  const [wholePart, fracPart = ""] = input.split(".");
  return { mantissa: BigInt(wholePart + fracPart), decimals: fracPart.length };
}

// multiplyToMinor computes qty × price as integer minor units (e.g. kopecks)
// with no floating-point arithmetic anywhere in the path. Both operands are
// non-negative decimal strings (trade quantity and price-per-unit); the
// caller applies the buy/sell sign afterwards.
//
// Algorithm: parse each operand into a BigInt mantissa + decimal-digit count,
// multiply the mantissas as BigInt (exact — no float rounding is possible),
// then reduce the combined decimal-digit count down to 2 (minor units) by
// truncating the excess digits. Sub-minor-unit remainders are dropped
// (truncation, not rounding): e.g. 1 × 0.001 = 0.001 of a unit, which
// truncates to 0 minor units. Returns null on malformed input or on overflow
// past Number.MAX_SAFE_INTEGER (BigInt math can't silently lose precision
// the way float math would, so overflow is detected exactly).
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
    // BigInt division truncates toward zero, which is exactly "truncate the
    // excess digits" for the non-negative values this function accepts.
    minorBig = productMantissa / 10n ** BigInt(totalDecimals - MINOR_DECIMALS);
  }

  if (minorBig > BigInt(Number.MAX_SAFE_INTEGER)) return null;
  const result = Number(minorBig);
  return Number.isSafeInteger(result) ? result : null;
}
