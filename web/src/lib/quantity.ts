// Quantities of shares, added exactly.
//
// A quantity is a count of shares, not money, and the screen adds them only to
// tell the owner how many of the shares that arrived the purchases already
// account for. Floats would say 0.1 + 0.2 is not 0.3; this works in integers of
// the journal's own scale (ten decimal places, NUMERIC(30,10)), so it agrees
// with the server about whether the parts make the whole.

const SCALE = 10;
const QUANTITY_RE = new RegExp(`^\\d+(\\.\\d{1,${SCALE}})?$`);

function scaled(value: string): bigint | null {
  if (!QUANTITY_RE.test(value)) return null;
  const [whole, frac = ""] = value.split(".");
  return BigInt(whole + frac.padEnd(SCALE, "0"));
}

function render(units: bigint): string {
  const digits = units.toString().padStart(SCALE + 1, "0");
  const whole = digits.slice(0, -SCALE);
  const frac = digits.slice(-SCALE).replace(/0+$/, "");
  return frac ? `${whole}.${frac}` : whole;
}

// normalizeQuantity accepts the comma a Russian keyboard types and returns the
// wire form, or the input unchanged when there is nothing to normalize.
export function normalizeQuantity(input: string): string {
  return input.trim().replace(",", ".");
}

// quantitySum adds quantities written as the wire expects them ("12.5"),
// returning the sum in the same form, or null if any part is not one.
export function quantitySum(parts: string[]): string | null {
  let total = 0n;
  for (const part of parts) {
    const units = scaled(part);
    if (units === null) return null;
    total += units;
  }
  return render(total);
}

// quantitiesMatch says whether the parts add up to exactly the whole.
export function quantitiesMatch(parts: string[], whole: string): boolean {
  const sum = quantitySum(parts);
  const target = scaled(whole);
  return sum !== null && target !== null && scaled(sum) === target;
}
