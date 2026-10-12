import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
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
import { PayoutsPage } from "./index";
import type { PayoutsForecast } from "@/api/payouts";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

const forecast: PayoutsForecast = {
  base_currency: "RUB",
  from: "2026-10-10",
  to: "2027-09-30",
  months: ["2026-10", "2026-11", "2026-12"],
  by_month: [0, 35_400, 91_800],
  total_minor: 127_200,
  net_by_month: [0, 30_798, 91_800],
  net_total_minor: 122_598,
  missing_rates: [],
  payouts: [
    { on: "2026-11-18", record_on: "2026-11-17", kind: "coupon", instrument_id: "i-ofz", account_id: "a-1", quantity: "10", per_unit: "35.4", amount_minor: 35_400, currency: "RUB", in_base_minor: 35_400, net_minor: 30_798, net_in_base_minor: 30_798, net_by: "rule" },
    { on: "2026-12-01", record_on: null, kind: "offer", instrument_id: "i-ofz", account_id: "a-1", quantity: "10", per_unit: null, amount_minor: null, currency: "RUB", in_base_minor: null, net_minor: null, net_in_base_minor: null, net_by: null },
    { on: "2026-12-15", record_on: "2026-11-28", kind: "dividend", instrument_id: "i-ko", account_id: "a-1", quantity: "20", per_unit: "0.51", amount_minor: 1_020, currency: "USD", in_base_minor: 91_800, net_minor: 1_020, net_in_base_minor: 91_800, net_by: "none" },
    { on: "2026-12-20", record_on: null, kind: "coupon", instrument_id: "i-float", account_id: "a-1", quantity: "5", per_unit: null, amount_minor: null, currency: "RUB", in_base_minor: null, net_minor: null, net_in_base_minor: null, net_by: null },
  ],
};

fetchMock.mockImplementation(async (input: Request) => {
  const path = new URL(input.url).pathname;
  const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
  if (path.endsWith("/payouts")) return json(forecast);
  if (path.endsWith("/accounts")) return json([]);
  if (path.endsWith("/instruments")) {
    return json({
      instruments: [
        { id: "i-ofz", type: "bond", name: "ОФЗ 26238", ticker: "SU26238RMFS4", isin: "", figi: "", currency: "RUB" },
        { id: "i-ko", type: "share", name: "Coca-Cola", ticker: "KO", isin: "", figi: "", currency: "USD" },
      ],
      has_more: false,
    });
  }
  return new Response("null", { status: 404 });
});

afterEach(() => {
  cleanup();
  localStorage.clear();
});

function renderPage() {
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const page = createRoute({ getParentRoute: () => rootRoute, path: "/payouts", component: PayoutsPage });
  const paper = createRoute({ getParentRoute: () => rootRoute, path: "/instruments/$instrumentId", component: () => null });
  const router = createRouter({
    routeTree: rootRoute.addChildren([page, paper]),
    history: createMemoryHistory({ initialEntries: ["/payouts"] }),
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("PayoutsPage", () => {
  it("lists the payouts by month, with what is not known yet said so", async () => {
    localStorage.setItem("babki.payoutView", "gross");
    renderPage();
    expect(await screen.findByTestId("payouts-total")).toHaveTextContent(/1\s272/);
    const months = await screen.findAllByTestId("payouts-month");
    expect(months.map((m) => m.querySelector("h3, [data-slot=card-title]")?.textContent ?? m.textContent?.slice(0, 14)))
      .toHaveLength(2);
    const rows = screen.getAllByTestId("payout-row");
    expect(rows[0]).toHaveTextContent("ОФЗ 26238");
    expect(rows[0]).toHaveTextContent("купон");
    expect(rows[0]).toHaveTextContent(/354/);
    expect(within(rows[1]).getByText("можно предъявить к выкупу")).toBeTruthy();
    expect(rows[2]).toHaveTextContent("Coca-Cola");
    expect(rows[2]).toHaveTextContent(/≈\s918/);
    expect(within(rows[3]).getByText("сумма ещё не объявлена")).toBeTruthy();
    expect(screen.queryByTestId("payout-net-by")).toBeNull();
  });

  it("shows what reaches the accounts unless picked otherwise, saying where it comes from", async () => {
    renderPage();
    expect(await screen.findByTestId("payouts-total")).toHaveTextContent(/1\s226/);
    const rows = await screen.findAllByTestId("payout-row");
    expect(rows[0].textContent?.replace(/\s/g, " ")).toMatch(/307,98 ₽−13 % по правилу · до налога 354,00 ₽/);
    // A payout in another currency: no tax known, the amount before tax.
    expect(within(rows[2]).getByTestId("payout-net-by").textContent).toBe("до налога: налог зависит от страны");
    expect(within(rows[1]).getByText("можно предъявить к выкупу")).toBeTruthy();

    fireEvent.click(screen.getByRole("combobox", { name: "Суммы" }));
    fireEvent.click(screen.getByRole("option", { name: "До налога" }));
    expect(screen.getByTestId("payouts-total")).toHaveTextContent(/1\s272/);
    expect(screen.queryByTestId("payout-net-by")).toBeNull();
    expect(localStorage.getItem("babki.payoutView")).toBe("gross");
  });
});
