import type { ReactNode } from "react";
import { describe, expect, it } from "vitest";
import { renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useInvalidateJournal } from "./operations";

// Everything read from a journal goes stale when one row changes: a deposit
// once left the account's return as it was until the page was reloaded.
describe("useInvalidateJournal", () => {
  it("marks stale every answer read from the account's journal, and only those", () => {
    const client = new QueryClient();
    const keys = [
      ["operations", "acc-1", 50, {}],
      ["positions", "acc-1"],
      ["arrivals", "acc-1", "paper-1"],
      ["accounts"],
      ["summary"],
      ["return", "acc-1", "2026-01-01", "2026-10-07"],
      ["return", "family", "2026-01-01", "2026-10-07"],
      ["return", "instrument", "paper-1", "2026-01-01", "2026-10-07"],
      ["capital", "2025-10-07"],
      ["instrument-holdings", "paper-1"],
      ["instrument-operations", "paper-1"],
    ];
    const untouched = [["positions", "acc-2"], ["members"], ["instrument-prices", "paper-1", "2026-01-01"]];
    for (const key of [...keys, ...untouched]) client.setQueryData(key, {});

    const { result } = renderHook(() => useInvalidateJournal(), {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    });
    result.current(["acc-1"]);

    for (const key of keys) {
      expect(client.getQueryState(key)?.isInvalidated, JSON.stringify(key)).toBe(true);
    }
    for (const key of untouched) {
      expect(client.getQueryState(key)?.isInvalidated, JSON.stringify(key)).toBe(false);
    }
  });
});
