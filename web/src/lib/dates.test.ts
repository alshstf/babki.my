import { describe, it, expect } from "vitest";
import { formatDate, formatDateTime, isRecent, localToday } from "./dates";

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

  // The whole suite runs at TZ=Europe/Moscow (+03:00, no daylight saving —
  // see vite.config.ts), so every expectation below is written out in full
  // rather than derived from another call to the function under test.
  describe("formatDateTime", () => {
    it("renders a UTC instant on the reader's own clock", () => {
      expect(formatDateTime("2026-08-04T09:15:00Z")).toBe("04.08.2026, 12:15");
    });

    it("carries an instant past midnight onto the next day", () => {
      expect(formatDateTime("2026-08-04T22:30:00Z")).toBe("05.08.2026, 01:30");
    });

    it("reads an instant sent with an offset rather than as UTC", () => {
      expect(formatDateTime("2026-08-04T09:15:00+01:00")).toBe("04.08.2026, 11:15");
    });

    it.each([[""], ["not-a-date"], ["2026-08-04T99:99:99Z"]])(
      "returns an empty string for %o rather than «Invalid Date»",
      (input) => {
        expect(formatDateTime(input)).toBe("");
      },
    );
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
