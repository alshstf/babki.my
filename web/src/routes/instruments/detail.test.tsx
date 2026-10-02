import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
import { InstrumentPage } from "./detail";
import { ScreenCurrencyCountProvider } from "@/lib/screen-currencies";
import type { SessionInfo } from "@/api/session";

// The double has to be in place before @/api/client captures fetch (see
// accounts/detail.test.tsx).
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const requested: string[] = [];

// Serves endpoints by the path's suffix and 404s the rest.
function serve(routes: Record<string, { status?: number; body?: unknown }>) {
  const paths = Object.keys(routes);
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = input instanceof Request ? input.url : String(input);
    requested.push(url);
    const path = new URL(url, "http://localhost").pathname;
    const match = paths.find((route) => path.endsWith(route));
    const route = match ? routes[match] : undefined;
    return Promise.resolve(
      new Response(JSON.stringify(route?.body ?? null), {
        status: route ? (route.status ?? 200) : 404,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

const session: SessionInfo = {
  user: { id: "user-1", username: "alex", display_name: "Alex" },
  role: "owner",
  space_id: "space-1",
  space_name: "Family",
  base_currency: "RUB",
  tax_residency: "RU",
  cost_basis_rules: { country: "RU", method: "fifo", perimeter: "account", supported: true, notices: [] },
};

const sber = {
  id: "sber",
  type: "share",
  name: "Сбербанк",
  ticker: "SBER",
  isin: "RU0009029540",
  figi: "",
  currency: "RUB",
  frozen: false,
};

function position(quantity: string, cost: number, value: number) {
  return {
    instrument: sber,
    quantity,
    cost_minor: cost,
    realized_pnl_minor: 0,
    income_minor: 0,
    income_by_currency: [],
    fees_minor: 0,
    currency: "RUB",
    has_undated_lots: false,
    has_undated_realizations: false,
    market_value_minor: value,
    market_value_currency: "RUB",
    price: "150",
    price_on: "2026-10-02",
  };
}

function account(id: string, name: string, status = "active") {
  return {
    id,
    name,
    type: "brokerage",
    currency: "RUB",
    institution: "",
    status,
    created_at: "2026-01-01T00:00:00Z",
    valued_by_balance: false,
    counted_by: "journal",
    balance: null,
  };
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  const rootRoute = createRootRoute();
  const layoutRoute = createRoute({
    getParentRoute: () => rootRoute,
    id: "app",
    component: () => (
      <ScreenCurrencyCountProvider>
        <Outlet />
      </ScreenCurrencyCountProvider>
    ),
  });
  const page = createRoute({ getParentRoute: () => layoutRoute, path: "/instruments/$instrumentId", component: InstrumentPage });
  const accountRoute = createRoute({ getParentRoute: () => layoutRoute, path: "/accounts/$accountId", component: () => null });
  const accounts = createRoute({ getParentRoute: () => layoutRoute, path: "/accounts", component: () => null });
  const router = createRouter({
    routeTree: rootRoute.addChildren([layoutRoute.addChildren([page, accountRoute, accounts])]),
    history: createMemoryHistory({ initialEntries: ["/instruments/sber"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  requested.length = 0;
});

describe("InstrumentPage", () => {
  it("names each account on its own row, links to it, says which is archived, and adds the server's total", async () => {
    serve({
      "/api/v1/accounts": { body: [account("a1", "Т-Банк"), account("a2", "Старый", "archived")] },
      "/holdings": {
        body: {
          instrument: sber,
          holdings: [
            { account_id: "a1", position: position("10", 100_000, 150_000) },
            { account_id: "a2", position: position("5", 60_000, 75_000) },
          ],
          total: { currency: "RUB", quantity: "15", cost_minor: 160_000, market_value_minor: 225_000, total_minor: 65_000 },
        },
      },
      "/prices": {
        body: [
          { on: "2026-10-01", price: "140", currency: "RUB", source: "moex_history" },
          { on: "2026-10-02", price: "150", currency: "RUB", source: "moex" },
        ],
      },
      "/operations": {
        body: {
          operations: [
            {
              id: "op-2", account_id: "a2", instrument_id: "sber", type: "buy", occurred_on: "2026-09-01",
              settled_on: null, quantity: "5", price: "120", amount_minor: -60_000, currency: "RUB", fee_minor: 0,
              note: "", source: "manual", created_at: "2026-09-01T00:00:00Z", has_undated_lots: false,
              assembled_from_lots: false, transfer_group_id: null, split_ratio: null,
            },
          ],
          has_more: false,
        },
      },
      "/return": {
        body: {
          currency: "RUB", from: "2025-10-03", to: "2026-10-03", start_minor: 0, end_minor: 225_000,
          contributions_minor: 160_000, profit_minor: 65_000, annual_rate: "0.4062", complete: true,
        },
      },
    });
    renderPage();

    expect(await screen.findByRole("heading", { name: /Сбербанк/ })).toBeInTheDocument();
    expect(screen.getByText(/SBER · RU0009029540/)).toBeInTheDocument();
    const first = await screen.findByRole("link", { name: "Т-Банк" });
    expect(first).toHaveAttribute("href", "/accounts/a1");
    const archivedRow = screen.getAllByRole("link", { name: "Старый" })[0].closest("tr") as HTMLElement;
    expect(within(archivedRow).getByText("архив")).toBeInTheDocument();
    expect(screen.getAllByRole("columnheader", { name: "Счёт" }).length).toBeGreaterThan(0);

    const total = screen.getByTestId("instrument-total");
    expect(total.textContent).toContain("Всего: 15 шт.");
    expect(total.textContent).toMatch(/стоимость 2\s250,00\s₽/);
    expect(total.textContent).toMatch(/итог 650,00\s₽/);

    const latest = await screen.findByTestId("instrument-latest-price");
    expect(latest.textContent).toMatch(/150,00\s₽/);
    expect(latest.textContent).toContain("Мосбиржа");
    expect(screen.getByTestId("price-chart")).toBeInTheDocument();

    expect(screen.getByRole("heading", { name: "Доходность бумаги" })).toBeInTheDocument();
    expect((await screen.findByTestId("account-return-profit")).textContent).toMatch(/650,00\s₽/);
    expect(requested.some((u) => u.includes("/api/v1/instruments/sber/return?from="))).toBe(true);

    const ops = await screen.findByTestId("paper-operations");
    expect(ops.textContent).toContain("Старый");
    expect(ops.textContent).toMatch(/−?-?600,00\s₽/);
  });

  it("asks for ten years of prices when the reader picks them", async () => {
    serve({
      "/api/v1/accounts": { body: [] },
      "/holdings": { body: { instrument: sber, holdings: [], total: null } },
      "/prices": { body: [] },
    });
    renderPage();
    expect(await screen.findByText("Этой бумаги нет ни на одном счёте семьи")).toBeInTheDocument();
    const yearFrom = () =>
      requested.filter((u) => u.includes("/prices")).map((u) => new URL(u, "http://localhost").searchParams.get("from"));
    await waitFor(() => expect(yearFrom()).toHaveLength(1));
    fireEvent.click(screen.getByRole("button", { name: "10 лет" }));
    await waitFor(() => expect(yearFrom()).toHaveLength(2));
    const [year, ten] = yearFrom().map((d) => Number(d?.slice(0, 4)));
    expect(year - ten).toBe(9);
  });

  it("offers a price by hand when there is none, and not over a fresh one from the exchange", async () => {
    const holdings = { body: { instrument: sber, holdings: [], total: null } };
    serve({ "/api/v1/accounts": { body: [] }, "/holdings": holdings, "/prices": { body: [] } });
    const first = renderPage();
    expect(await screen.findByRole("button", { name: "указать текущую цену" })).toBeInTheDocument();
    first.unmount();

    const today = new Date().toISOString().slice(0, 10);
    serve({
      "/api/v1/accounts": { body: [] },
      "/holdings": holdings,
      "/prices": { body: [{ on: today, price: "150", currency: "RUB", source: "moex" }] },
    });
    renderPage();
    expect(await screen.findByTestId("instrument-latest-price")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "указать текущую цену" })).toBeNull();
  });

  it("says the paper is not found on a 404", async () => {
    serve({ "/api/v1/accounts": { body: [] } });
    renderPage();
    expect(await screen.findByText("Бумага не найдена")).toBeInTheDocument();
  });
});
