import { describe, expect, it } from "vitest";
import { resolveDisplayAmount } from "./display-amount";

describe("resolveDisplayAmount", () => {
  it("shows the native amount unchanged in native mode, even when a base figure is available", () => {
    const resolved = resolveDisplayAmount("native", "USD", 100_00, "RUB", 950_000, "2026-07-20");
    expect(resolved).toEqual({
      amountMinor: 100_00,
      currency: "USD",
      noRate: false,
      converted: false,
      rateOn: null,
    });
  });

  it("shows the converted amount in base mode when a base figure is available", () => {
    const resolved = resolveDisplayAmount("base", "USD", 100_00, "RUB", 950_000);
    expect(resolved).toEqual({
      amountMinor: 950_000,
      currency: "RUB",
      noRate: false,
      // Converted, and said so without a rate date: a converted figure does
      // not always have one date behind it (a position's cost is struck at
      // one rate per purchase day), and a caller that inferred "converted"
      // from the date would stay silent about a conversion that happened.
      converted: true,
      rateOn: null,
    });
  });

  it("carries the fx rate date through when the converted amount is what gets shown", () => {
    const resolved = resolveDisplayAmount("base", "USD", 100_00, "RUB", 950_000, "2026-07-20");
    expect(resolved).toEqual({
      amountMinor: 950_000,
      currency: "RUB",
      noRate: false,
      converted: true,
      rateOn: "2026-07-20",
    });
  });

  it("falls back to the native amount with noRate=true in base mode when no base figure is available", () => {
    const resolved = resolveDisplayAmount("base", "USD", 100_00, "RUB", null, "2026-07-20");
    expect(resolved).toEqual({
      amountMinor: 100_00,
      currency: "USD",
      noRate: true,
      converted: false,
      // No rate date on a figure that was never converted, even if the
      // caller passed one: it would describe a conversion that didn't happen.
      rateOn: null,
    });
  });

  it("treats undefined the same as null for the base figure", () => {
    const resolved = resolveDisplayAmount("base", "USD", 100_00, "RUB", undefined);
    expect(resolved.noRate).toBe(true);
  });

  it("shows the native amount with no noRate flag when the native currency already equals the base currency, even without a base figure", () => {
    // The backend never populates in_base/balance_in_base when currency ===
    // base_currency (nothing to convert) — null here does NOT mean "missing
    // rate", so no honesty indicator should appear.
    const resolved = resolveDisplayAmount("base", "RUB", 100_00, "RUB", null);
    expect(resolved).toEqual({
      amountMinor: 100_00,
      currency: "RUB",
      noRate: false,
      converted: false,
      rateOn: null,
    });
  });

  it("prefers the native currency check over a (theoretically impossible) base figure when currencies already match", () => {
    const resolved = resolveDisplayAmount("base", "RUB", 100_00, "RUB", 999_00, "2026-07-20");
    expect(resolved).toEqual({
      amountMinor: 100_00,
      currency: "RUB",
      noRate: false,
      converted: false,
      rateOn: null,
    });
  });

  it("treats a zero base amount as present (not missing)", () => {
    const resolved = resolveDisplayAmount("base", "USD", 100_00, "RUB", 0, "2026-07-20");
    expect(resolved).toEqual({
      amountMinor: 0,
      currency: "RUB",
      noRate: false,
      converted: true,
      rateOn: "2026-07-20",
    });
  });
});
