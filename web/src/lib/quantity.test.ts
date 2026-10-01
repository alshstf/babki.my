import { describe, expect, it } from "vitest";
import { normalizeQuantity, quantitiesMatch, quantitySum } from "./quantity";

describe("quantities", () => {
  it("adds exactly, where floats would not", () => {
    expect(quantitySum(["0.1", "0.2"])).toBe("0.3");
    expect(quantitiesMatch(["0.1", "0.2"], "0.3")).toBe(true);
    expect(quantitySum(["4", "6"])).toBe("10");
    expect(quantitySum(["0.0000000001", "9.9999999999"])).toBe("10");
  });

  it("says when the parts are not the whole", () => {
    expect(quantitiesMatch(["4", "5"], "10")).toBe(false);
    expect(quantitiesMatch(["4", "6"], "10.0")).toBe(true);
  });

  it("refuses what is not a quantity", () => {
    expect(quantitySum(["4", "six"])).toBeNull();
    expect(quantitySum(["0.00000000001"])).toBeNull();
    expect(quantitiesMatch([], "10")).toBe(false);
  });

  it("takes the comma a Russian keyboard types", () => {
    expect(normalizeQuantity(" 12,5 ")).toBe("12.5");
  });
});
