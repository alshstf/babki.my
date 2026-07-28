import { describe, expect, it } from "vitest";
import { formatMinor, formatMinorCompact, multiplyToMinor, parseToMinor, isPositiveDecimal } from "./money";

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
