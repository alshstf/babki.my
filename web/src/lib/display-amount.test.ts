import { describe, expect, it } from "vitest";
import { resolveDisplayAmount } from "./display-amount";

describe("resolveDisplayAmount", () => {
  it("shows the native amount unchanged in native mode, even when a base figure is available", () => {
    const resolved = resolveDisplayAmount("native", "USD", 100_00, "RUB", {
      amountMinor: 950_000,
      currency: "RUB",
      rateOn: "2026-07-20",
    });
    expect(resolved).toEqual({
      amountMinor: 100_00,
      currency: "USD",
      noRate: false,
      converted: false,
      rateOn: null,
    });
  });

  it("shows the converted amount in base mode when a base figure is available", () => {
    const resolved = resolveDisplayAmount("base", "USD", 100_00, "RUB", {
      amountMinor: 950_000,
      currency: "RUB",
    });
    expect(resolved).toEqual({
      amountMinor: 950_000,
      currency: "RUB",
      noRate: false,
      // Converted, said without a rate date: a position's cost has one rate per
      // purchase day, so the date cannot decide "converted".
      converted: true,
      rateOn: null,
    });
  });

  it("takes the converted figure's currency from the figure, never from the session", () => {
    // #106: the figure's own currency wins over the session's. After a base
    // change, cached figures stay in the old currency until refetched; the
    // server publishes every converted figure's currency (required in the
    // contract), so this reads what was sent.
    const resolved = resolveDisplayAmount("base", "USD", 100_00, "EUR", {
      amountMinor: 950_000,
      currency: "RUB",
      rateOn: "2026-07-20",
    });
    expect(resolved.currency).toBe("RUB");
    expect(resolved.amountMinor).toBe(950_000);
    expect(resolved.converted).toBe(true);
  });

  it("carries the fx rate date through when the converted amount is what gets shown", () => {
    const resolved = resolveDisplayAmount("base", "USD", 100_00, "RUB", {
      amountMinor: 950_000,
      currency: "RUB",
      rateOn: "2026-07-20",
    });
    expect(resolved).toEqual({
      amountMinor: 950_000,
      currency: "RUB",
      noRate: false,
      converted: true,
      rateOn: "2026-07-20",
    });
  });

  it("falls back to the native amount with noRate=true in base mode when no base figure is available", () => {
    const resolved = resolveDisplayAmount("base", "USD", 100_00, "RUB", {
      amountMinor: null,
      currency: "RUB",
      rateOn: "2026-07-20",
    });
    expect(resolved).toEqual({
      amountMinor: 100_00,
      currency: "USD",
      noRate: true,
      converted: false,
      // No rate date on an unconverted figure, even if one is passed.
      rateOn: null,
    });
  });

  it("treats an absent conversion block the same as an absent amount inside one", () => {
    // Nothing published comes in two shapes: in_base null, or the one term
    // this cell wants null (e.g. market_value_minor).
    expect(resolveDisplayAmount("base", "USD", 100_00, "RUB", null).noRate).toBe(true);
    expect(resolveDisplayAmount("base", "USD", 100_00, "RUB", undefined).noRate).toBe(true);
    expect(
      resolveDisplayAmount("base", "USD", 100_00, "RUB", {
        amountMinor: undefined,
        currency: "RUB",
      }).noRate,
    ).toBe(true);
  });

  it("shows the native amount with no noRate flag when the native currency already equals the base currency, even without a base figure", () => {
    // In the base currency there is nothing to convert and in_base is never
    // sent: null is not a missing rate. Only the session can answer this.
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
    const resolved = resolveDisplayAmount("base", "RUB", 100_00, "RUB", {
      amountMinor: 999_00,
      currency: "RUB",
      rateOn: "2026-07-20",
    });
    expect(resolved).toEqual({
      amountMinor: 100_00,
      currency: "RUB",
      noRate: false,
      converted: false,
      rateOn: null,
    });
  });

  it("treats a zero base amount as present (not missing)", () => {
    const resolved = resolveDisplayAmount("base", "USD", 100_00, "RUB", {
      amountMinor: 0,
      currency: "RUB",
      rateOn: "2026-07-20",
    });
    expect(resolved).toEqual({
      amountMinor: 0,
      currency: "RUB",
      noRate: false,
      converted: true,
      rateOn: "2026-07-20",
    });
  });
});
