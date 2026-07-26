import { describe, expect, it } from "vitest";
import { formatMinor, formatMinorCompact, parseToMinor } from "./money";

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
