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
      // Converted, and said so without a rate date: a converted figure does
      // not always have one date behind it (a position's cost is struck at
      // one rate per purchase day), and a caller that inferred "converted"
      // from the date would stay silent about a conversion that happened.
      converted: true,
      rateOn: null,
    });
  });

  it("takes the converted figure's currency from the figure, never from the session", () => {
    // #106. The two arguments disagree, and the figure wins — because it is
    // the figure that carries the arithmetic. The disagreement is not a
    // contrived one: change the base currency in settings and every cache
    // holding figures converted into the OLD one is stale for as long as it
    // takes to refetch (see useUpdateSpace, which invalidates exactly for
    // this), while the session's new answer lands at once. Labelling those
    // rubles as dollars would not merely mislabel them — it would print a
    // number that is wrong by the whole exchange rate, with nothing on screen
    // saying so.
    //
    // The server publishes the currency of every converted figure and marks it
    // required — MoneyInBase.currency, OperationInBase.currency,
    // PositionInBase.currency — so this is reading what was sent, not
    // inferring anything.
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
      // No rate date on a figure that was never converted, even if the
      // caller passed one: it would describe a conversion that didn't happen.
      rateOn: null,
    });
  });

  it("treats an absent conversion block the same as an absent amount inside one", () => {
    // The two shapes a caller has for "the server published nothing to show":
    // in_base itself is null (the usual one — the whole object is withheld
    // together), or it exists and the one term this cell wants is null (a
    // position's market_value_minor, which can be missing on its own).
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
    // The backend never populates in_base/balance_in_base when currency ===
    // base_currency (nothing to convert) — null here does NOT mean "missing
    // rate", so no honesty indicator should appear. This is the one question
    // the session's base currency answers and the figure cannot: there is no
    // figure to read a currency off when the server published none.
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
