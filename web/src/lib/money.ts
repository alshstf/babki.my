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
