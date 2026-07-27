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

export function formatMinorCompact(amountMinor: number, currency: string): string {
  return formatWith(amountMinor, currency, 0);
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
