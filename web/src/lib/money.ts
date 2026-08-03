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

// A quote below a hundredth, but not zero: whole part 0 — possibly written
// with extra leading zeros ("00.0001"), which this function's own validator
// two lines below accepts (`\d+` matches any run of digits) — fraction
// starting "00", and a non-zero digit somewhere in it. Without the `0*`
// prefix, "00.0001" would fall through to the ordinary two-fraction-digit
// branch and print the fake zero this regex exists to prevent; the leading
// zeros are never on the wire (decimal.String() emits none), but the
// contract is the regex, not the sender — same standard as the underflow
// guard below. Matched on the DIGITS rather than on the parsed double on
// purpose — see formatPrice.
const SUB_CENT_PRICE_RE = /^0*0\.00\d*[1-9]/;

// How many digits of a sub-cent price are worth showing. Three is the same
// order of detail two fraction digits give an ordinary quote (95,20 is four
// significant digits, 0,05 is one), enough to tell 0,000123 from 0,000456 —
// and unlike a fixed fraction-digit count it needs no ceiling: the price
// picks its own scale.
const SUB_CENT_SIGNIFICANT_DIGITS = 3;

// formatPrice renders a raw decimal-string price (a quote, not minor units)
// as a ru-RU number. A price of a hundredth or more gets exactly two fraction
// digits, e.g. "305.5" -> "305,50" — the ordinary share and bond quote, and
// what every quote in the demo seed but one is.
//
// Below a hundredth those two digits would print "0,00" (#30): a number that
// is neither the price nor zero, one cell away from the column where this
// program refuses to publish a figure it cannot vouch for. So a sub-cent
// price is rendered by significant digits instead — "0.0001" -> "0,0001",
// "0.000123456" -> "0,000123" — and never as a zero. That branch is not held
// in reserve for a currency this program has yet to meet: it runs every time
// the demo stand draws the Freedom KZ account's positions, where WeWork is
// quoted at $0.0025 after its bankruptcy (cmd/babki/seed.go), and a delisted
// share reaches these digits the same way a coin does.
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

// How many fraction digits a derived price or percentage is written with at
// minimum — two, so 980 prints as "980.00" and 98 as "98.00", the way a quote
// is written everywhere else on this screen and in the broker's terminal the
// number was copied out of. It is a MINIMUM and never a ceiling: renderDecimal
// keeps every digit the exact value actually has, because the money side of
// this pair is on its way into a cost basis and a dropped digit there is money
// the position never gets back.
const MIN_DERIVED_FRACTION_DIGITS = 2;

// renderDecimal writes an exact BigInt mantissa with `decimals` decimal places
// as a plain decimal string, trailing zeros trimmed down to (but never below)
// MIN_DERIVED_FRACTION_DIGITS. The point separator is "." rather than the
// Russian ",", because what this produces goes straight into an <input> whose
// contents are validated by isPositiveDecimal and sent on the wire — it is a
// value, not a rendering, and formatPrice is what turns a value into Russian.
function renderDecimal(mantissa: bigint, decimals: number): string {
  const digits = mantissa.toString().padStart(decimals + 1, "0");
  const whole = digits.slice(0, digits.length - decimals);
  let frac = decimals === 0 ? "" : digits.slice(digits.length - decimals);
  frac = frac.replace(/0+$/, "");
  return `${whole}.${frac.padEnd(MIN_DERIVED_FRACTION_DIGITS, "0")}`;
}

// bondPriceFromPercent converts an exchange's bond quote — a PERCENTAGE OF
// FACE VALUE, e.g. "98" — into the money one bond costs, as a decimal string
// in the face value's currency: 98 % of a 1 000,00 ₽ face is "980.00".
//
// This is #77. A broker quotes a bond in percent and nothing about the number
// says so: 98 is a perfectly good price, a form that asks for «цена за
// единицу» accepts it without a murmur, and 10 bonds bought at it record
// 980 ₽ instead of 9 800 ₽ — a cost basis ten times too small, and the
// expense side of a tax calculation with it.
//
// It agrees with the server's own statement of the same arithmetic
// (marketValue in internal/portfolio/http.go: faceValueMinor × price/100 ×
// quantity, in the FACE currency) by doing exactly its first two factors and
// stopping there — the quantity is applied afterwards by multiplyToMinor,
// which is where this trade's single rounding happens. Not "agrees to within
// a rounding step": THIS function rounds nothing at all. The exact product of
// an integer number of minor units and a finite decimal percentage is itself
// a finite decimal, so it is written out in full however many digits that
// takes, and the only place a fraction of a kopeck can be dropped is the
// total — once, exactly as it always was for a share.
//
// Returns null rather than a number whenever there is no conversion to
// perform: a malformed percentage, or a face value that is absent, zero or
// negative. A zero face value is refused rather than multiplied, because
// 0 × anything is 0 and a fabricated zero in a price field is precisely the
// plausible-looking number this project does not publish. The caller's job is
// then to say what is missing — never to show the percentage as if it were
// money.
export function bondPriceFromPercent(percentOfFace: string, faceValueMinor: number): string | null {
  const percent = parseDecimalString(percentOfFace);
  if (!percent) return null;
  if (!Number.isSafeInteger(faceValueMinor) || faceValueMinor <= 0) return null;
  // Two divisions by a hundred, and they are different divisions: one takes
  // the face value from minor units into major ones, the other takes the
  // quote from percent into a fraction. Folding them into a single /100 is
  // the off-by-a-hundred this whole function exists to prevent, so they are
  // spelled out as two separate decimal shifts of two each.
  return renderDecimal(BigInt(faceValueMinor) * percent.mantissa, percent.decimals + 2 + 2);
}

// How many fraction digits the derived PERCENTAGE is computed to. Ten matches
// the scale prices are stored at (NUMERIC(30,10)) and the ceiling
// isPositiveDecimal enforces on what the field will accept back, so a value
// this function produces is always one the form will take.
const PERCENT_FRACTION_DIGITS = 10;

// bondPercentFromPrice is the same conversion read backwards: given the money
// one bond costs, what percentage of face is the exchange quoting? 980 ₽
// against a 1 000,00 ₽ face is "98.00".
//
// The direction matters for where a rounding may live. Percent → money is
// always exact (see above) and is the figure that gets recorded; money →
// percent need not be, because a face value whose denominator is not built
// out of 2s and 5s makes the quotient non-terminating — 100 ₽ against a
// 3,00 ₽ face is 3333,333… %. That last digit is therefore rounded
// half-up at PERCENT_FRACTION_DIGITS, and it is allowed to be: a percentage
// is not money (the same standing exception that lets the positions screen
// compute a profit percentage in the browser), and this particular percentage
// is a caption on a number the user typed rather than the number itself. What
// gets sent is always the money price, exactly as entered.
//
// Rounded half-away-from-zero rather than truncated, which for a
// non-negative quotient is floor(n/d + 1/2) — written as (2n + d) / 2d so the
// halving happens in integers and no double is created on the way.
export function bondPercentFromPrice(pricePerUnit: string, faceValueMinor: number): string | null {
  const price = parseDecimalString(pricePerUnit);
  if (!price) return null;
  if (!Number.isSafeInteger(faceValueMinor) || faceValueMinor <= 0) return null;
  // percent = price / (faceValueMinor / 100) × 100 = price × 10000 / faceValueMinor.
  const numerator = price.mantissa * 10_000n * 10n ** BigInt(PERCENT_FRACTION_DIGITS);
  const denominator = BigInt(faceValueMinor) * 10n ** BigInt(price.decimals);
  return renderDecimal((2n * numerator + denominator) / (2n * denominator), PERCENT_FRACTION_DIGITS);
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
