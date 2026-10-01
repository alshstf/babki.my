import { describe, expect, it } from "vitest";
import { newPurchaseRow, purchasesReady, rowProblem, toStatedPurchase, type PurchaseRow } from "./purchases-editor";

const row = (patch: Partial<PurchaseRow>): PurchaseRow => ({ ...newPurchaseRow(), ...patch });

describe("a purchase row", () => {
  it("is sent as a price per share for the server to strike, never as a sum the screen worked out", () => {
    expect(toStatedPurchase(row({ quantity: "4", price: "250,50", acquiredOn: "2021-03-02" }))).toEqual({
      quantity: "4",
      price: "250.50",
      cost_minor: null,
      fee_minor: 0,
      acquired_on: "2021-03-02",
    });
  });

  it("is sent as a total, with its commission, when only the total is known", () => {
    expect(toStatedPurchase(row({ quantity: "6", total: "1 800,00", fee: "1,50" }))).toEqual({
      quantity: "6",
      price: null,
      cost_minor: 180_000,
      fee_minor: 150,
      acquired_on: null,
    });
  });

  it("names what keeps it from being sent", () => {
    const arrived = "2026-06-15";
    expect(rowProblem(row({ quantity: "", price: "1" }), arrived)).toBe("quantity");
    expect(rowProblem(row({ quantity: "1" }), arrived)).toBe("priceOrTotal");
    expect(rowProblem(row({ quantity: "1", price: "1", total: "1" }), arrived)).toBe("priceOrTotal");
    expect(rowProblem(row({ quantity: "1", price: "abc" }), arrived)).toBe("price");
    expect(rowProblem(row({ quantity: "1", total: "-5" }), arrived)).toBe("total");
    expect(rowProblem(row({ quantity: "1", price: "1", fee: "x" }), arrived)).toBe("fee");
    expect(rowProblem(row({ quantity: "1", price: "1", acquiredOn: "2026-06-16" }), arrived)).toBe("date");
    expect(rowProblem(row({ quantity: "1", price: "0", acquiredOn: "2026-06-15" }), arrived)).toBeNull();
  });
});

describe("purchases", () => {
  it("are ready only when every row is sound and together they are exactly the shares that arrived", () => {
    const arrived = "2026-06-15";
    const a = row({ quantity: "0,1", price: "10" });
    const b = row({ quantity: "0.2", price: "10" });
    expect(purchasesReady([a, b], arrived, "0.3")).toBe(true);
    expect(purchasesReady([a], arrived, "0.3")).toBe(false);
    expect(purchasesReady([a, row({ quantity: "0.2" })], arrived, "0.3")).toBe(false);
    expect(purchasesReady([], arrived, "0.3")).toBe(false);
  });
});
