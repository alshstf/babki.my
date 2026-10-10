import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { AccountReturn, FamilyReturnLine } from "./account-return";
import { formatMinor } from "@/lib/money";

const asked = vi.hoisted(() => [] as URL[]);
const state = vi.hoisted(() => ({ complete: true, accounts: 2 }));
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});
fetchMock.mockImplementation(async (input: Request) => {
  const url = new URL(input.url);
  asked.push(url);
  if (url.pathname.endsWith("/return/benchmarks")) {
    return new Response(
      JSON.stringify({
        currency: "RUB", from: "2025-10-03", to: "2026-10-03",
        benchmarks: [
          { code: "MCFTR", index_currency: "RUB", complete: true, end_minor: 16_900_000, annual_rate: "0.0811" },
          { code: "RGBITR", index_currency: "RUB", complete: false, end_minor: null, annual_rate: null },
          { code: "SP500TR", index_currency: "USD", complete: true, end_minor: 18_100_000, annual_rate: "0.1433" },
        ],
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  }
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
      time_weighted_rate: "0.0864",
      time_weighted_period: "0.0864",
      complete: state.complete,
      accounts: state.accounts,
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
});

afterEach(() => {
  cleanup();
  asked.length = 0;
  state.complete = true;
  state.accounts = 2;
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
    expect(norm(screen.getByTestId("account-return-twr").textContent ?? "")).toBe("+8,6 %");
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

  it("shows the family's return over its brokerage accounts, and nothing without any", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { unmount } = render(
      <QueryClientProvider client={client}>
        <FamilyReturnLine />
      </QueryClientProvider>,
    );
    expect(await screen.findByText(/счетов: 2/)).toBeTruthy();
    expect(asked[0].pathname).toBe("/api/v1/return");
    unmount();

    state.accounts = 0;
    const empty = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={empty}>
        <FamilyReturnLine />
      </QueryClientProvider>,
    );
    await waitFor(() => expect(asked.filter((u) => u.pathname === "/api/v1/return")).toHaveLength(2));
    expect(screen.queryByTestId("account-return")).toBeNull();
  });

  it("weighs the family's return against the indices priced over the period", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <FamilyReturnLine />
      </QueryClientProvider>,
    );
    expect(norm((await screen.findByTestId("return-benchmarks")).textContent ?? "")).toBe(
      "Если бы те же деньги шли в индекс, годовых: акции РФ (MCFTR) +8,1 % · S&P 500 с дивидендами +14,3 %",
    );
  });
});
