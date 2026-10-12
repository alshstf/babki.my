import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import "@/i18n";
import { PaperPayouts } from "./paper-payouts";

const asked = vi.hoisted(() => [] as URL[]);
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});
let payouts: unknown[] = [];
fetchMock.mockImplementation(async (input: Request) => {
  const url = new URL(input.url);
  asked.push(url);
  return new Response(
    JSON.stringify({
      base_currency: "RUB", from: "2026-10-10", to: "2027-09-30", months: [], by_month: [], total_minor: 0,
      missing_rates: [], payouts,
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
});

afterEach(() => {
  cleanup();
  asked.length = 0;
  localStorage.clear();
});

function renderIt() {
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const page = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: () => <PaperPayouts instrumentId="i-ofz" accountName={() => "Брокер"} />,
  });
  const all = createRoute({ getParentRoute: () => rootRoute, path: "/payouts", component: () => null });
  const router = createRouter({
    routeTree: rootRoute.addChildren([page, all]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("PaperPayouts", () => {
  it("lists the paper's own payouts for a year", async () => {
    payouts = [
      { on: "2026-12-02", record_on: null, kind: "coupon", instrument_id: "i-ofz", account_id: "a-1", quantity: "100", per_unit: "35.4", amount_minor: 354_000, currency: "RUB", in_base_minor: 354_000, net_minor: 307_980, net_in_base_minor: 307_980, net_by: "rule" },
    ];
    renderIt();
    // What reaches the account by default, the amount before tax beside it.
    expect(await screen.findByTestId("paper-payout")).toHaveTextContent(/02\.12\.2026.*купон.*Брокер.*3\s079,80.*−13 % по правилу · до налога 3\s540,00/);
    expect(asked.some((u) => u.searchParams.get("instrument_id") === "i-ofz" && u.searchParams.get("months") === "12")).toBe(true);
  });

  it("follows the view picked on the payouts page", async () => {
    localStorage.setItem("babki.payoutView", "gross");
    payouts = [
      { on: "2026-12-02", record_on: null, kind: "coupon", instrument_id: "i-ofz", account_id: "a-1", quantity: "100", per_unit: "35.4", amount_minor: 354_000, currency: "RUB", in_base_minor: 354_000, net_minor: 307_980, net_in_base_minor: 307_980, net_by: "rule" },
    ];
    renderIt();
    const row = await screen.findByTestId("paper-payout");
    expect(row).toHaveTextContent(/3\s540,00/);
    expect(row).not.toHaveTextContent(/3\s079/);
  });

  it("says nothing when nothing is announced", async () => {
    payouts = [];
    renderIt();
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByTestId("paper-payouts")).toBeNull();
  });
});
