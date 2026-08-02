import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { renderHook } from "@testing-library/react";
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
import { AccountsPage } from "./index";
import { ScreenCurrencyCountProvider } from "@/lib/screen-currencies";
import { useDisplayCurrency } from "@/lib/display-currency";
import type { SessionInfo } from "@/api/session";
import type { AccountWithBalance, Summary } from "@/api/accounts";

// The API client captures globalThis.fetch once, when @/api/client is first
// imported (openapi-fetch: `fetch: baseFetch = globalThis.fetch`), so the
// double has to be in place *before* that import — hence vi.hoisted, which
// runs ahead of the import statements above.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Serves the given endpoints (matched as URL substrings) and 404s the rest,
// so an unexpected request is loud rather than silently hanging.
function serve(routes: Record<string, { status?: number; body?: unknown }>) {
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = input instanceof Request ? input.url : String(input);
    const match = Object.keys(routes).find((path) => url.includes(path));
    const route = match ? routes[match] : undefined;
    return Promise.resolve(
      new Response(JSON.stringify(route?.body ?? null), {
        status: route ? (route.status ?? 200) : 404,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

function makeSession(): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role: "owner",
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    tax_residency: "RU",
    cost_basis_rules: {
      country: "RU",
      method: "fifo",
      perimeter: "account",
      supported: true,
      notices: [],
    },
  };
}

function makeAccount(overrides: Partial<AccountWithBalance> = {}): AccountWithBalance {
  return {
    id: "acc-1",
    name: "Наличные",
    type: "cash",
    currency: "RUB",
    institution: "",
    status: "active",
    created_at: "2026-01-01T00:00:00Z",
    balance: { as_of: "2026-07-20", amount_minor: 100_000 },
    ...overrides,
  };
}

function makeSummary(overrides: Partial<Summary> = {}): Summary {
  return {
    totals: [
      { currency: "RUB", assets_minor: 100_000, liabilities_minor: 0, net_minor: 100_000 },
    ],
    base_currency: "RUB",
    total_in_base_minor: 100_000,
    unconverted: [],
    rates_on: null,
    ...overrides,
  };
}

// Renders AccountsPage the way AppLayout does: inside the screen-currency
// provider (which the page reports into) and a router (its rows link to the
// detail route).
function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], makeSession());
  const rootRoute = createRootRoute({
    component: () => (
      <ScreenCurrencyCountProvider>
        <Outlet />
      </ScreenCurrencyCountProvider>
    ),
  });
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: AccountsPage,
  });
  const detailRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/accounts/$accountId",
    component: () => null,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute, detailRoute]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

// Sets the user's stored display-currency choice the way the header toggle
// does (the store is module-level, shared by every consumer in the process).
function storeMode(mode: "native" | "base") {
  const { result, unmount } = renderHook(() => useDisplayCurrency());
  act(() => result.current.setMode(mode));
  unmount();
}

describe("AccountsPage — display currency mode", () => {
  beforeEach(() => {
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/api/v1/summary": { body: makeSummary() },
    });
  });

  afterEach(() => {
    storeMode("native");
    window.localStorage.clear();
  });

  it("keeps showing the per-currency breakdown when only one currency is on screen, even with base mode stored", async () => {
    // The reviewer's trap: the user turns on "base" on a multi-currency
    // screen, later lands on a screen with a single currency — where the
    // toggle hides itself — and the breakdown cards disappear with no
    // control anywhere on screen to bring them back.
    storeMode("base");
    renderPage();

    expect(await screen.findByText("Итого в RUB")).toBeInTheDocument();
    // The account's own balance is shown natively too, for the same reason.
    await waitFor(() =>
      expect(screen.getByTestId("account-balance-acc-1")).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("account-balance-acc-1-not-converted")).not.toBeInTheDocument();
  });

  it("applies the stored base mode once the screen really has two currencies", async () => {
    serve({
      "/api/v1/accounts": {
        body: [
          makeAccount({
            id: "acc-2",
            currency: "USD",
            balance: { as_of: "2026-07-20", amount_minor: 10_000 },
            balance_in_base: { amount_minor: 900_000, currency: "RUB", rate_on: "2026-07-20" },
          }),
        ],
      },
      "/api/v1/summary": { body: makeSummary() },
    });
    storeMode("base");
    renderPage();

    const amount = await screen.findByTestId("account-balance-acc-2");
    // 9 000,00 ₽ — the backend's converted figure, not the native $100.00.
    expect(amount.textContent).toMatch(/₽/);
    // ...and the per-currency breakdown steps aside, as it does in base mode.
    expect(screen.queryByText("Итого в RUB")).not.toBeInTheDocument();
  });
});
