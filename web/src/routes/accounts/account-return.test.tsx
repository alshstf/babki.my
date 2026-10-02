import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { AccountReturn } from "./account-return";
import { formatMinor } from "@/lib/money";

const asked = vi.hoisted(() => [] as URL[]);
const state = vi.hoisted(() => ({ complete: true }));
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});
fetchMock.mockImplementation(async (input: Request) => {
  asked.push(new URL(input.url));
  return new Response(
    JSON.stringify({
      currency: "RUB",
      from: "2025-10-03",
      to: "2026-10-03",
      start_minor: 10_000_000,
      end_minor: 17_400_000,
      contributions_minor: 6_050_000,
      profit_minor: 1_350_000,
      annual_rate: "0.1052",
      complete: state.complete,
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
});

afterEach(() => {
  cleanup();
  asked.length = 0;
  state.complete = true;
});

const norm = (s: string) => s.replace(/[  ]/g, " ");

function wrap() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <AccountReturn accountId="acc-1" />
    </QueryClientProvider>,
  );
}

describe("AccountReturn", () => {
  it("shows the year's profit and annual rate, and asks again for another period", async () => {
    wrap();
    expect(norm((await screen.findByTestId("account-return-profit")).textContent ?? "")).toBe(
      norm(formatMinor(1_350_000, "RUB")),
    );
    expect(norm(screen.getByTestId("account-return-rate").textContent ?? "")).toBe("+10,5 %");
    expect(asked[0].pathname).toBe("/api/v1/accounts/acc-1/return");
    const to = asked[0].searchParams.get("to") ?? "";
    expect(asked[0].searchParams.get("from")).toBe(`${Number(to.slice(0, 4)) - 1}${to.slice(4)}`);
    expect(screen.queryByTestId("account-return-incomplete")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "с начала года" }));
    await waitFor(() => expect(asked).toHaveLength(2));
    expect(asked[1].searchParams.get("from")).toBe(`${Number(to.slice(0, 4)) - 1}-12-31`);
  });

  it("shows no figure for a period valued only in part, and says why", async () => {
    state.complete = false;
    wrap();
    expect(await screen.findByTestId("account-return-incomplete")).toBeTruthy();
    expect(screen.queryByTestId("account-return-profit")).toBeNull();
    expect(screen.queryByTestId("account-return-rate")).toBeNull();
  });
});
