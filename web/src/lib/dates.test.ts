import { describe, it, expect } from "vitest";
import { formatDate, isRecent, localToday } from "./dates";

describe("dates", () => {
  describe("formatDate", () => {
    it("formats an ISO date as short ru-RU", () => {
      expect(formatDate("2026-07-20")).toBe("20.07.2026");
    });

    it.each([[""], ["not-a-date"], ["2026-13-99"]])(
      "returns empty string for junk input %s, not \"Invalid Date\"",
      (input) => {
        expect(formatDate(input)).toBe("");
      },
    );

    it("returns empty string for null-ish input", () => {
      // @ts-expect-error exercising runtime guard against non-string input
      expect(formatDate(null)).toBe("");
      // @ts-expect-error exercising runtime guard against non-string input
      expect(formatDate(undefined)).toBe("");
    });
  });

  describe("isRecent", () => {
    it("treats today as recent", () => {
      expect(isRecent("2026-07-20", "2026-07-20")).toBe(true);
    });

    it("treats yesterday as recent", () => {
      expect(isRecent("2026-07-19", "2026-07-20")).toBe(true);
    });

    it("treats a year rollover correctly", () => {
      expect(isRecent("2025-12-31", "2026-01-01")).toBe(true);
    });

    it("treats two days ago as not recent", () => {
      expect(isRecent("2026-07-18", "2026-07-20")).toBe(false);
    });

    it("treats a future date as not recent", () => {
      expect(isRecent("2026-07-21", "2026-07-20")).toBe(false);
    });
  });

  describe("localToday", () => {
    it("returns a date in YYYY-MM-DD format", () => {
      const result = localToday();
      expect(result).toMatch(/^\d{4}-\d{2}-\d{2}$/);
    });

    it("returns the local calendar date", () => {
      const result = localToday();
      const now = new Date();
      const y = now.getFullYear();
      const m = String(now.getMonth() + 1).padStart(2, "0");
      const day = String(now.getDate()).padStart(2, "0");
      const expected = `${y}-${m}-${day}`;
      expect(result).toBe(expected);
    });
  });
});
