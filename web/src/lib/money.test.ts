import shared from "./testdata/trade-amounts.json";
import { describe, expect, it } from "vitest";
import {
  bondPercentFromPrice,
  bondPriceFromPercent,
  formatMinor,
  formatMinorCompact,
  formatPrice,
  formatPriceIn,
  multiplyToMinor,
  parseToMinor,
  amountRefusal,
  isPositiveDecimal,
  MAX_AMOUNT_MINOR,
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
  it("writes a currency with no fraction whole, unless the amount has one", () => {
    expect(norm(formatMinor(123_400, "JPY"))).toBe("1 234 JPY");
    expect(norm(formatMinor(123_450, "JPY"))).toBe("1 234,50 JPY");
    expect(norm(formatMinor(123_400, "KWD"))).toBe("1 234,00 KWD");
  });
});

describe("formatMinorCompact", () => {
  it("drops kopecks", () => {
    expect(norm(formatMinorCompact(138_500_000, "RUB"))).toBe("1 385 000 ₽");
  });

  it("keeps abbreviating millions, thousands and whole units exactly as before", () => {
    // At or above half a major unit the kopecks are still dropped and
    // rounded: 1 234,56 reads 1 235.
    expect(norm(formatMinorCompact(1_234_56, "RUB"))).toBe("1 235 ₽");
    expect(norm(formatMinorCompact(-1_234_56, "RUB"))).toBe("-1 235 ₽");
    expect(norm(formatMinorCompact(99, "RUB"))).toBe("1 ₽");
    expect(norm(formatMinorCompact(50, "RUB"))).toBe("1 ₽");
  });

  it("shows a sum too small to survive the abbreviation instead of printing it as zero", () => {
    // #107: forty kopecks printed «0 ₽», a green zero on the total card. A
    // money amount is an integer of minor units, so full precision is always
    // two digits.
    expect(norm(formatMinorCompact(40, "RUB"))).toBe("0,40 ₽");
    expect(norm(formatMinorCompact(1, "RUB"))).toBe("0,01 ₽");
    expect(norm(formatMinorCompact(49, "RUB"))).toBe("0,49 ₽");
    // A currency this program has no symbol for takes the same branch.
    expect(norm(formatMinorCompact(40, "XXX"))).toBe("0,40 XXX");
  });

  it("does not print a minus zero for a small debt either", () => {
    // The negative side survives formatWith's -0 guard (-40 is not zero), so
    // it would print «-0 ₽».
    expect(norm(formatMinorCompact(-40, "RUB"))).toBe("-0,40 ₽");
    expect(norm(formatMinorCompact(-1, "RUB"))).toBe("-0,01 ₽");
    expect(norm(formatMinorCompact(-49, "RUB"))).toBe("-0,49 ₽");
  });

  it("prints a real zero as a zero", () => {
    expect(norm(formatMinorCompact(0, "RUB"))).toBe("0 ₽");
    expect(norm(formatMinorCompact(-0, "RUB"))).toBe("0 ₽");
  });

  it("renders no non-zero amount the way it renders zero, at any magnitude", () => {
    // The property: a non-zero sum never prints as zero does.
    const zero = formatMinorCompact(0, "RUB");
    for (const amountMinor of [
      1, 5, 40, 49, 50, 51, 99, 100, 1_00, 999_99, 1_385_000_00,
      -1, -5, -40, -49, -50, -51, -99, -100, -1_00, -999_99, -1_385_000_00,
    ]) {
      expect(formatMinorCompact(amountMinor, "RUB")).not.toBe(zero);
    }
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

// #89: the balance endpoint sent a sum past exact doubles as a different
// number. The server refuses past MAX_AMOUNT_MINOR; the field refuses at
// the keystroke.
describe("parseToMinor at the bound", () => {
  it("takes the largest sum there is, exactly", () => {
    // The bound is exact on both signs: a debt is as recordable as an asset.
    expect(parseToMinor("10000000000000")).toBe(MAX_AMOUNT_MINOR);
    expect(parseToMinor("-10000000000000")).toBe(-MAX_AMOUNT_MINOR);
    expect(parseToMinor("9999999999999,99")).toBe(MAX_AMOUNT_MINOR - 1);
  });

  it("refuses one kopeck past it, either sign", () => {
    expect(parseToMinor("10000000000000,01")).toBeNull();
    expect(parseToMinor("-10000000000000,01")).toBeNull();
  });

  it("refuses a sum a double could not carry", () => {
    // 10^17 kopecks: past Number.MAX_SAFE_INTEGER.
    expect(parseToMinor("1000000000000000")).toBeNull();
    // And a whole part no double can hold at all, which computes to Infinity.
    expect(parseToMinor("9".repeat(400))).toBeNull();
  });

  it("stays below the point where a double stops being exact", () => {
    // The property the bound is chosen for: every parsed value is exact.
    expect(MAX_AMOUNT_MINOR).toBeLessThan(Number.MAX_SAFE_INTEGER);
  });
});

describe("amountRefusal", () => {
  it("tells a number that cannot be sent from text that is not a number", () => {
    // Two different sentences: "не удалось разобрать" for a well-formed
    // sum would be false.
    expect(amountRefusal("10000000000000,01")).toBe("tooLarge");
    expect(amountRefusal("abc")).toBe("malformed");
    expect(amountRefusal("")).toBe("malformed");
  });

  it("is null for everything parseToMinor accepts", () => {
    for (const good of ["0", "-0", "1 234,56", "-92 000", "10000000000000"]) {
      expect(amountRefusal(good)).toBeNull();
      expect(parseToMinor(good)).not.toBeNull();
    }
  });

  it("never disagrees with parseToMinor", () => {
    // Both come from one parse, so a field never disables its button
    // without a reason or explains a working one.
    for (const input of [
      "0",
      "abc",
      "1 234,56",
      "10000000000000",
      "10000000000000,01",
      "-10000000000000,01",
      "1.2.3",
      "",
      "9".repeat(400),
    ]) {
      expect(parseToMinor(input) === null).toBe(amountRefusal(input) !== null);
    }
  });
});

describe("formatPrice", () => {
  // The ordinary case pinned digit for digit, separator included: the
  // sub-cent branch must not touch it.
  it.each([
    ["305.567", "305,57"],
    ["100", "100,00"],
    ["95.20", "95,20"],
    ["1234.5", "1 234,50"],
    ["0.01", "0,01"],
    // A quote that really is zero is not a fake zero, and still prints as one.
    ["0", "0,00"],
    ["0.00", "0,00"],
    // The sub-cent threshold's upper edge: a price in [0.01, 0.1) stays on
    // the two-digit branch. Only this value tells "0.0" from "0.00" in the
    // regex.
    ["0.0567", "0,06"],
  ])("formats %s as %s", (input, want) => {
    expect(norm(formatPrice(input) ?? "")).toBe(want);
  });

  it.each([[""], ["-5"], ["1e5"], ["abc"], ["1,5"]])("rejects %s", (input) => {
    expect(formatPrice(input)).toBeNull();
  });

  // #30: below a hundredth, significant digits instead of a fake "0,00".
  // Compared exactly: "0" is in nearly every result.
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

  // Extra leading zeros pass the input check, so the sub-cent regex must
  // match them too. Unreachable from the wire, but the regex decides.
  it("shows significant digits, not a fake zero, for a sub-cent price with a leading zero", () => {
    const got = norm(formatPrice("00.0001") ?? "");
    expect(got).toBe("0,0001");
    expect(got).not.toBe("0,00");
  });

  // A decimal that underflows the double to zero has no digits left: no
  // hint, as for any input that cannot be rendered.
  it("omits the hint entirely for a price too small to have any digits left", () => {
    expect(formatPrice("0." + "0".repeat(400) + "1")).toBeNull();
  });
});

// formatPrice with the quote currency named (#76); the digit rules are
// shared, so a value reads the same with or without a sign.
describe("formatPriceIn", () => {
  it.each([
    ["305.5", "USD", "305,50 $"],
    ["274.49", "RUB", "274,49 ₽"],
    ["1234.5", "EUR", "1 234,50 €"],
  ])("writes %s in %s as %s", (input, currency, want) => {
    expect(norm(formatPriceIn(input, currency) ?? "")).toBe(want);
  });

  // The demo WeWork quote ($0.0025, cmd/babki/seed.go): a currency style
  // would give it USD's two digits and print «0,00 $».
  it("keeps the significant digits of a sub-cent price and still names the currency", () => {
    const got = norm(formatPriceIn("0.0025", "USD") ?? "");
    expect(got).toBe("0,0025 $");
    expect(got).not.toBe("0,00 $");
  });

  // A code outside the shared list is appended plain, as formatMinor
  // does, so a currency reads the same in a price and a money cell.
  it("appends the plain code for a currency the money formatter does not style", () => {
    expect(norm(formatPriceIn("305.5", "JPY") ?? "")).toBe("305,50 JPY");
  });

  // Refusal is inherited: the caller drops the whole hint.
  it.each([[""], ["-5"], ["abc"], ["1,5"]])("rejects %s exactly as formatPrice does", (input) => {
    expect(formatPriceIn(input, "USD")).toBeNull();
    expect(formatPrice(input)).toBeNull();
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
  // Held to one table with the Go test of operation.TradeAmountMinor.
  it.each(shared.cases)("agrees with the server on $quantity × $price", ({ quantity, price, minor }) => {
    expect(multiplyToMinor(quantity, price)).toBe(minor);
  });

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

  it("rounds many fractional digits up rather than dropping them", () => {
    // 12,9999999999 kopecks round to 13, as decimal.Round(0) does in Go.
    expect(multiplyToMinor("1", "0.129999999999")).toBe(13);
  });

  // #94: 98,0005 % of a 1 000,00 ₽ face is 98 000,5 kopecks for one bond;
  // the server (money.Minor, half away from zero) says 98 001.
  it("rounds a half kopeck the way the server rounds it", () => {
    const price = bondPriceFromPercent("98.0005", 100_000);
    expect(price).toBe("980.005");
    expect(multiplyToMinor("1", price ?? "")).toBe(98_001);
  });

  it.each([
    // Exactly half a minor unit, away from zero — the whole rule in one case.
    ["1", "0.005", 1],
    // Just under and over the half, so rounding everything up fails.
    ["1", "0.0049", 0],
    ["1", "0.0051", 1],
    // Half a kopeck on top of a whole one: 1,5 kopecks → 2, not 1.
    ["1", "0.015", 2],
    // 0,005 of a kopeck is nowhere near half of one.
    ["1", "0.00005", 0],
  ])("rounds %s × %s half away from zero", (qty, price, want) => {
    expect(multiplyToMinor(qty, price)).toBe(want);
  });

  // A buy negates this magnitude (trade-dialog.tsx); rounding the
  // magnitude half away from zero first gives −98 001, never −98 000, as
  // money.Minor does in Go.
  it("never shrinks the magnitude a buy will negate", () => {
    const magnitude = multiplyToMinor("1", "980.005");
    expect(magnitude).toBe(98_001);
    expect(-(magnitude ?? 0)).toBe(-98_001);
  });

  it("handles many decimal digits on both operands without precision loss", () => {
    // Exact BigInt math: far below half a unit rounds to 0, not NaN.
    expect(multiplyToMinor("0.123456789", "0.000000001")).toBe(0);
  });

  it("returns null on overflow past Number.MAX_SAFE_INTEGER", () => {
    expect(multiplyToMinor("100000000", "100000000")).toBeNull();
  });

  it("returns null on overflow even with fractional operands", () => {
    expect(multiplyToMinor("99999999999.99", "99999999999.99")).toBeNull();
  });

  it("accepts a large-but-safe product", () => {
    // 9 000 000 rubles, safely under Number.MAX_SAFE_INTEGER.
    expect(multiplyToMinor("90000", "100")).toBe(900_000_000);
  });
});

// An exchange quotes a bond as a percentage of face (#77): 98 of a
// 1 000 ₽ face is 980 ₽. Both directions are exact integer arithmetic,
// since the money side becomes a cost basis.
describe("bondPriceFromPercent", () => {
  // The owner's case from the issue; anything but 980 is the
  // ten-times-too-small basis.
  it("turns the owner's 98 % of a 1 000 ₽ face into 980 per bond", () => {
    expect(bondPriceFromPercent("98", 100_000)).toBe("980.00");
  });

  it.each([
    // The face's scale must not leak: these pin the two /100 steps
    // separately.
    ["98", 10_000, "98.00"],
    ["98", 100, "0.98"],
    // Par, premium and deep discount against the 1 000 ₽ face.
    ["100", 100_000, "1000.00"],
    ["104.5", 100_000, "1045.00"],
    ["7.25", 100_000, "72.50"],
    // Needed fraction digits are kept, not rounded: nothing rounds here, so
    // the total rounds once, in multiplyToMinor.
    ["98.005", 100_000, "980.05"],
    ["33.3333", 100_000, "333.333"],
    ["33.3333", 100, "0.333333"],
  ])("converts %s %% of a face of %d minor units", (percent, faceMinor, want) => {
    expect(bondPriceFromPercent(percent, faceMinor)).toBe(want);
  });

  // No usable face, no conversion: the caller renders the absence, never
  // a 0.
  it.each([[0], [-100_000]])("refuses a face value of %d", (faceMinor) => {
    expect(bondPriceFromPercent("98", faceMinor)).toBeNull();
  });

  it.each([[""], ["abc"], ["-5"], ["9,8"], ["1e2"]])("refuses percent %s", (percent) => {
    expect(bondPriceFromPercent(percent, 100_000)).toBeNull();
  });

  // 0 % of any face is 0, a plausible price this project does not publish.
  it.each([["0"], ["0.00"]])("refuses a percentage of %s", (percent) => {
    expect(bondPriceFromPercent(percent, 100_000)).toBeNull();
  });

  // Whatever comes back, the price field's validator accepts; a price
  // finer than storable would need rounding money, so it is refused.
  it("refuses a percentage whose exact price is finer than a stored price", () => {
    // Twelve fraction digits, two past what isPositiveDecimal accepts.
    expect(bondPriceFromPercent("98.0000000001", 100)).toBeNull();
  });

  it("still converts the finest percentage whose price does fit", () => {
    // Exactly ten, the widest accepted, so the refusal cannot creep inward.
    expect(bondPriceFromPercent("98.00000001", 100)).toBe("0.9800000001");
  });

  it.each([
    ["98", 100_000],
    ["33.3333", 100],
    ["98.00000001", 100],
    // Trailing zeros are trimmed, so the rendered width is what counts.
    ["98.000000", 100_000],
  ])("returns %s of a face of %d minor units as a price the form takes back", (percent, faceMinor) => {
    const price = bondPriceFromPercent(percent, faceMinor);
    expect(price).not.toBeNull();
    expect(isPositiveDecimal(price as string)).toBe(true);
  });
});

describe("bondPercentFromPrice", () => {
  // The owner's case backwards, with a quote's two fraction digits.
  it("turns 980 per bond against a 1 000 ₽ face back into 98 %", () => {
    expect(bondPercentFromPrice("980", 100_000)).toBe("98.00");
  });

  it.each([
    ["98", 10_000, "98.00"],
    ["0.98", 100, "98.00"],
    ["1000", 100_000, "100.00"],
    // The third digit is kept, not rounded to 98,38.
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

  // The one allowed rounding, on the percentage (a ratio, not money).
  // Real faces divide exactly, so this needs hand-entered data. A 6,00 ₽
  // face repeats 6s, so half-away and truncation give different strings.
  it("rounds a non-terminating percentage rather than truncating it", () => {
    expect(bondPercentFromPrice("100", 600)).toBe("1666.6666666667");
  });
});

// Whatever is typed in one field, the other describes the same trade;
// a hundredfold error there and back passes each one-way test.
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
