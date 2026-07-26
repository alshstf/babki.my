import { describe, it, expect } from "vitest";
import { localToday } from "./dates";

describe("dates", () => {
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
