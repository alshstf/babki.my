import { describe, expect, it } from "vitest";
import {
  bondPercentFromPrice,
  bondPriceFromPercent,
  formatMinor,
  formatMinorCompact,
  formatPrice,
  multiplyToMinor,
  parseToMinor,
  isPositiveDecimal,
} from "./money";

// NBSP-insensitive compare: Intl uses non-breaking spaces.
const norm = (s: string) => s.replace(/[  ]/g, " ");

describe("formatMinor", () => {
  it("formats RUB with kopecks", () => {
    expect(norm(formatMinor(138_500_000, "RUB"))).toBe("1 385 000,00 ₽");
  });
  it("formats negative USD", () => {
    expect(norm(formatMinor(-9_200_00, "USD"))).toContain("-9 200,00");
    expect(formatMinor(-9_200_00, "USD")).toContain("$");
  });
  it("formats zero as positive, not -0", () => {
    expect(norm(formatMinor(0, "RUB"))).toBe("0,00 ₽");
    expect(norm(formatMinor(-0, "RUB"))).toBe("0,00 ₽");
  });
  it("falls back for unknown currency", () => {
    const out = norm(formatMinor(1_00, "XXX"));
    expect(out).toContain("1,00");
    expect(out).toContain("XXX");
  });
});

describe("formatMinorCompact", () => {
  it("drops kopecks", () => {
    expect(norm(formatMinorCompact(138_500_000, "RUB"))).toBe("1 385 000 ₽");
  });
});

describe("parseToMinor", () => {
  it.each([
    ["1 234,56", 123_456],
    ["1234.56", 123_456],
    ["-92 000", -9_200_000],
    ["0", 0],
    ["-0", 0],
    ["1 385 000,5", 138_500_050],
  ])("parses %s", (input, want) => {
    const result = parseToMinor(input);
    expect(result).toBe(want);
    // Ensure -0 is normalized to +0, not IEEE -0
    if (want === 0) {
      expect(Object.is(result, 0)).toBe(true);
    }
  });
  it.each([["abc"], [""], ["12,34,56"], ["1.2.3"]])("rejects %s", (input) => {
    expect(parseToMinor(input)).toBeNull();
  });
});

describe("formatPrice", () => {
  // The ordinary case, pinned digit for digit: every quote this program has
  // met so far is a hundredth or more, and none of them may move because of
  // what the sub-cent branch below does. Thousands separator included — the
  // adaptive branch, applied to the whole range, would print 1234.5 as
  // "1 230".
  it.each([
    ["305.567", "305,57"],
    ["100", "100,00"],
    ["95.20", "95,20"],
    ["1234.5", "1 234,50"],
    ["0.01", "0,01"],
    // A quote that really is zero is not a fake zero, and still prints as one.
    ["0", "0,00"],
    ["0.00", "0,00"],
    // Pins the sub-cent threshold's UPPER edge. SUB_CENT_PRICE_RE requires
    // "0.00" right after the point; a price in [0.01, 0.1) has a non-"00"
    // pair there and must stay on the ordinary two-fraction-digit branch. The
    // nearby "0.01" case above doesn't pin this: it happens to format the
    // same whether or not the regex is widened to match "0.0" instead of
    // "0.00". This one, with three fraction digits and a value that only the
    // ordinary branch rounds this way, does not.
    ["0.0567", "0,06"],
  ])("formats %s as %s", (input, want) => {
    expect(norm(formatPrice(input) ?? "")).toBe(want);
  });

  it.each([[""], ["-5"], ["1e5"], ["abc"], ["1,5"]])("rejects %s", (input) => {
    expect(formatPrice(input)).toBeNull();
  });

  // #30: two fraction digits turn any price below a hundredth into "0,00" — a
  // number that is neither the price nor zero, printed one cell away from the
  // column where this program refuses to show a figure it cannot vouch for.
  // Below a hundredth the price is rendered by significant digits instead.
  // Compared exactly, not by substring: "0" is a substring of nearly every
  // number this function returns.
  it.each([
    ["0.0001", "0,0001"],
    ["0.000123456", "0,000123"],
    ["0.005", "0,005"],
    ["0.0099", "0,0099"],
    ["0.00000001234", "0,0000000123"],
  ])("shows the significant digits of sub-cent price %s as %s", (input, want) => {
    const got = norm(formatPrice(input) ?? "");
    expect(got).toBe(want);
    expect(got).not.toBe("0,00");
  });

  // formatPrice's own input validator (`/^\d+(\.\d+)?$/`) accepts extra
  // leading zeros before the point, but SUB_CENT_PRICE_RE used to anchor on
  // a literal "0\." and so never matched "00.0001" — that input fell to the
  // ordinary branch and printed the fake "0,00" this whole function exists to
  // avoid. Unreachable from the wire (decimal.String() never emits a leading
  // zero), but out of reach is not the same contract as "cannot happen": the
  // regex is what decides, same as the double-underflow guard just below.
  it("shows significant digits, not a fake zero, for a sub-cent price with a leading zero", () => {
    const got = norm(formatPrice("00.0001") ?? "");
    expect(got).toBe("0,0001");
    expect(got).not.toBe("0,00");
  });

  // The one input this function accepts that has no honest rendering at all:
  // a decimal string so small it underflows the double to exactly zero, so
  // there are no significant digits left to show. The server cannot send one
  // (quotes are NUMERIC(30,10), and 1e-10 is nowhere near the underflow
  // boundary), but this function's contract is its own regex, not its
  // caller's table — and omitting the hint is what it already does for every
  // other input it cannot render.
  it("omits the hint entirely for a price too small to have any digits left", () => {
    expect(formatPrice("0." + "0".repeat(400) + "1")).toBeNull();
  });
});

describe("isPositiveDecimal", () => {
  it.each([["1"], ["0.5"], ["1234.1234567890"]])("accepts %s", (input) => {
    expect(isPositiveDecimal(input)).toBe(true);
  });
  it.each([[""], ["0"], ["-1"], ["abc"], ["1.12345678901"]])("rejects %s", (input) => {
    expect(isPositiveDecimal(input)).toBe(false);
  });
});

describe("multiplyToMinor", () => {
  it.each([
    ["10", "305.5", 305_500],
    ["0.5", "100", 5_000],
    ["3", "0.01", 3],
    ["1", "0.001", 0], // sub-kopeck rounds to zero — allowed, documented
  ])("multiplies %s × %s", (qty, price, want) => {
    expect(multiplyToMinor(qty, price)).toBe(want);
  });
  it.each([["abc", "1"], ["1", ""], ["-1", "10"]])("rejects %s × %s", (q, p) => {
    expect(multiplyToMinor(q, p)).toBeNull();
  });

  // Additional edge cases beyond the brief's literal table.
  it.each([
    ["0", "305.5", 0],
    ["10", "0", 0],
    ["0", "0", 0],
  ])("treats zero operand %s × %s as 0", (qty, price, want) => {
    expect(multiplyToMinor(qty, price)).toBe(want);
  });

  it("truncates many fractional digits instead of rounding", () => {
    // 1 × 0.129999999999 = 0.129999999999 of a unit → truncates to 12 minor
    // units, not 13 — confirms truncation semantics hold beyond 2 combined
    // decimal digits of input.
    expect(multiplyToMinor("1", "0.129999999999")).toBe(12);
  });

  it("handles many decimal digits on both operands without precision loss", () => {
    // Exact BigInt math: 0.123456789 × 0.000000001 = 0.000000000123456789,
    // far below one minor unit — truncates to 0, not NaN/Infinity as float
    // multiplication of such small magnitudes might risk.
    expect(multiplyToMinor("0.123456789", "0.000000001")).toBe(0);
  });

  it("returns null on overflow past Number.MAX_SAFE_INTEGER", () => {
    expect(multiplyToMinor("100000000", "100000000")).toBeNull();
  });

  it("returns null on overflow even with fractional operands", () => {
    expect(multiplyToMinor("99999999999.99", "99999999999.99")).toBeNull();
  });

  it("accepts a large-but-safe product", () => {
    // 90_000 × 100 = 9_000_000 rubles = 900_000_000 minor units, safely
    // under Number.MAX_SAFE_INTEGER (~9.007e15).
    expect(multiplyToMinor("90000", "100")).toBe(900_000_000);
  });
});

// The two halves of the bond quote convention (#77): an exchange quotes a bond
// as a PERCENTAGE OF FACE VALUE, so 98 against a 1 000 ₽ face means 980 ₽ per
// bond. Both directions are exact integer arithmetic — the whole point of the
// pair is that the money side of it is a figure the user is about to record as
// a cost basis, and a float would put an invented kopeck in it.
describe("bondPriceFromPercent", () => {
  // THE case from the issue, digit for digit: the owner copies 98 out of his
  // broker's terminal for an OFZ with a 1 000,00 ₽ face. Anything but 980
  // here is the ten-times-too-small cost basis the whole task exists to end.
  it("turns the owner's 98 % of a 1 000 ₽ face into 980 per bond", () => {
    expect(bondPriceFromPercent("98", 100_000)).toBe("980.00");
  });

  it.each([
    // A face value's own scale must not leak into the answer: the same 98 %
    // against a 100,00 ₽ face is 98 ₽, against a 1,00 ₽ face 0,98 ₽. These
    // pin the two /100 steps (minor→major, percent→fraction) separately —
    // dropping either turns 980,00 into 98 000,00 or 9,80.
    ["98", 10_000, "98.00"],
    ["98", 100, "0.98"],
    // Par, premium and deep discount against the 1 000 ₽ face.
    ["100", 100_000, "1000.00"],
    ["104.5", 100_000, "1045.00"],
    ["7.25", 100_000, "72.50"],
    // Fraction digits the money price genuinely needs are kept, not rounded
    // away: 98,005 % of 1 000 ₽ is 980,05 ₽ exactly, 33,3333 % is 333,333 ₽
    // exactly, and 33,3333 % of a 1,00 ₽ face is 0,333333 ₽ exactly — six
    // digits, kept. No rounding happens anywhere in this conversion, which is
    // what lets the total round exactly once, in multiplyToMinor.
    ["98.005", 100_000, "980.05"],
    ["33.3333", 100_000, "333.333"],
    ["33.3333", 100, "0.333333"],
  ])("converts %s %% of a face of %d minor units", (percent, faceMinor, want) => {
    expect(bondPriceFromPercent(percent, faceMinor)).toBe(want);
  });

  // Honesty over silence, in its arithmetic form: with no usable face value
  // there is no conversion to perform, and 0 would be a fabricated answer
  // rather than an absent one. The caller renders the absence; it must never
  // receive a number here.
  it.each([[0], [-100_000]])("refuses a face value of %d", (faceMinor) => {
    expect(bondPriceFromPercent("98", faceMinor)).toBeNull();
  });

  it.each([[""], ["abc"], ["-5"], ["9,8"], ["1e2"]])("refuses percent %s", (percent) => {
    expect(bondPriceFromPercent(percent, 100_000)).toBeNull();
  });

  // Zero is not malformed input, but it is the same fabricated-answer problem
  // as a zero face value read the other way round: 0 % of any face is 0, a
  // plausible-looking price this project does not publish either.
  it.each([["0"], ["0.00"]])("refuses a percentage of %s", (percent) => {
    expect(bondPriceFromPercent(percent, 100_000)).toBeNull();
  });

  // The guarantee this direction owes the form, and the one the percentage
  // direction has had all along: whatever comes back, the price field's own
  // validator takes. An exact price finer than a price is stored has no honest
  // rendering here — rounding it would drop money out of a cost basis — so the
  // conversion is refused, and the caller is left with the absence it already
  // knows how to show. Handing back a value the form rejects would instead
  // make the dialog complain about a price field the user never typed in and
  // cannot correct from the percentage he did type.
  it("refuses a percentage whose exact price is finer than a stored price", () => {
    // 98,0000000001 % of a 1,00 ₽ face is 0,980000000001 ₽ exactly: twelve
    // fraction digits, two past what isPositiveDecimal accepts.
    expect(bondPriceFromPercent("98.0000000001", 100)).toBeNull();
  });

  it("still converts the finest percentage whose price does fit", () => {
    // Exactly ten fraction digits — the last accepted width, pinned so the
    // refusal above cannot creep inward and start refusing storable prices.
    expect(bondPriceFromPercent("98.00000001", 100)).toBe("0.9800000001");
  });

  it.each([
    ["98", 100_000],
    ["33.3333", 100],
    ["98.00000001", 100],
    // Ten decimal places of arithmetic, two digits of answer: the trailing
    // zeros are trimmed, so the width that matters is the rendered one.
    ["98.000000", 100_000],
  ])("returns %s of a face of %d minor units as a price the form takes back", (percent, faceMinor) => {
    const price = bondPriceFromPercent(percent, faceMinor);
    expect(price).not.toBeNull();
    expect(isPositiveDecimal(price as string)).toBe(true);
  });
});

describe("bondPercentFromPrice", () => {
  // The other direction of the owner's own case: 980 ₽ per bond against a
  // 1 000 ₽ face is 98 %, and the field must say so with the two fraction
  // digits a quote is written with.
  it("turns 980 per bond against a 1 000 ₽ face back into 98 %", () => {
    expect(bondPercentFromPrice("980", 100_000)).toBe("98.00");
  });

  it.each([
    ["98", 10_000, "98.00"],
    ["0.98", 100, "98.00"],
    ["1000", 100_000, "100.00"],
    // A price with kopecks in it: 983,75 ₽ of a 1 000 ₽ face is 98,375 %,
    // exactly — the third digit is kept rather than rounded to 98,38.
    ["983.75", 100_000, "98.375"],
  ])("converts a price of %s against a face of %d minor units", (price, faceMinor, want) => {
    expect(bondPercentFromPrice(price, faceMinor)).toBe(want);
  });

  it.each([[0], [-100_000]])("refuses a face value of %d", (faceMinor) => {
    expect(bondPercentFromPrice("980", faceMinor)).toBeNull();
  });

  it.each([[""], ["abc"], ["-5"], ["9,8"]])("refuses price %s", (price) => {
    expect(bondPercentFromPrice(price, 100_000)).toBeNull();
  });

  // The one place a rounding is allowed to appear in this pair, and it is on
  // the PERCENTAGE — a ratio, not money (the project's standing exception).
  // A face value whose denominator is not built from 2s and 5s makes the
  // percentage non-terminating: 100 ₽ per bond against a 6,00 ₽ face is
  // 1666,666… %. Every face value a real bond carries (1, 10, 100, 1 000,
  // 10 000 units) divides exactly, so this branch is reachable only from
  // hand-entered data — and it still may not print a wrong digit, only a
  // rounded last one.
  //
  // The 6,00 ₽ face is the fixture on purpose: a repeating 6 makes the digit
  // past the tenth round the last one UP, so half-away-from-zero and plain
  // truncation give different strings and only one of them passes. The 3,00 ₽
  // face this case used to carry cannot tell them apart — a repeating 3 gives
  // "3333.3333333333" under either rule, which pinned the digit count and
  // nothing whatever about the rounding.
  it("rounds a non-terminating percentage rather than truncating it", () => {
    expect(bondPercentFromPrice("100", 600)).toBe("1666.6666666667");
  });
});

// The pair as a whole: whatever the user types into one field, the other must
// describe the SAME trade, and the money one must survive a round trip.
// A conversion that is off by a factor of a hundred in one direction and back
// would pass each single-direction assertion above; this catches it.
describe("bond price and percent round-trip", () => {
  it.each([
    ["98", 100_000],
    ["104.5", 100_000],
    ["98.375", 100_000],
    ["7.25", 10_000],
  ])("returns to %s %% through the money price", (percent, faceMinor) => {
    const price = bondPriceFromPercent(percent, faceMinor);
    expect(price).not.toBeNull();
    const back = bondPercentFromPrice(price as string, faceMinor);
    expect(Number(back)).toBe(Number(percent));
  });
});
