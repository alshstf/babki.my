import { describe, expect, it } from "vitest";
import { parseReceiptQr, receiptIsIncoming } from "./receipt-qr";

describe("parseReceiptQr", () => {
  it("reads a purchase: the total in kopecks, the day and the minute", () => {
    expect(parseReceiptQr("t=20261010T1230&s=1234.50&fn=7380440700000000&i=12345&fp=1234567890&n=1")).toEqual({
      amountMinor: 1_234_50, date: "2026-10-10", time: "12:30", kind: "purchase",
      fn: "7380440700000000", fd: "12345", fp: "1234567890",
    });
  });

  it("takes the fields in any order, a time with seconds and a total without kopecks", () => {
    const r = parseReceiptQr("n=2&fp=99&i=7&fn=1&s=171&t=20190718T131645");
    expect(r).toMatchObject({ amountMinor: 171_00, date: "2019-07-18", time: "13:16", kind: "refund" });
    expect(receiptIsIncoming(r!.kind)).toBe(true);
  });

  it("reads a single digit of kopecks as tens", () => {
    expect(parseReceiptQr("t=20261010T1230&s=99.5&fn=1&i=2&fp=3&n=1")?.amountMinor).toBe(99_50);
  });

  it("is not a receipt without any of its fields, or with one malformed", () => {
    for (const text of [
      "https://example.org",
      "t=20261010T1230&s=10.00&fn=1&i=2&n=1",
      "t=20261010T1230&s=10.00&fn=1&i=2&fp=3&n=9",
      "t=2026-10-10&s=10.00&fn=1&i=2&fp=3&n=1",
      "t=20261310T1230&s=10.00&fn=1&i=2&fp=3&n=1",
      "t=20261010T1230&s=-5&fn=1&i=2&fp=3&n=1",
      "t=20261010T1230&s=0.00&fn=1&i=2&fp=3&n=1",
    ]) {
      expect(parseReceiptQr(text), text).toBeNull();
    }
  });
});
