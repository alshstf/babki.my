import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, renderHook, screen } from "@testing-library/react";
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
import { AccountDetailPage } from "./detail";
import { ScreenCurrencyCountProvider } from "@/lib/screen-currencies";
import { useDisplayCurrency } from "@/lib/display-currency";
import type { SessionInfo } from "@/api/session";
import type { AccountWithBalance } from "@/api/accounts";

// The API client captures globalThis.fetch once, when @/api/client is first
// imported (openapi-fetch: `fetch: baseFetch = globalThis.fetch`), so the
// double has to be in place *before* that import — hence vi.hoisted, which
// runs ahead of the import statements above.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Serves the given endpoints and 404s everything else, so an unexpected
// request is loud rather than silent. Routes match on the path's *suffix*,
// not on a substring: "/api/v1/accounts/acc-1/positions" contains
// "/api/v1/accounts", so substring matching would quietly serve the account
// body for a positions request and leave the table rendering nothing.
function serve(routes: Record<string, { status?: number; body?: unknown }>) {
  const paths = Object.keys(routes);
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = input instanceof Request ? input.url : String(input);
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

function makeSession(overrides: Partial<SessionInfo> = {}): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role: "owner",
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    ...overrides,
  };
}

function makeAccount(overrides: Partial<AccountWithBalance> = {}): AccountWithBalance {
  return {
    id: "acc-1",
    name: "Брокерский",
    type: "brokerage",
    currency: "USD",
    institution: "Broker Co",
    status: "active",
    created_at: "2026-01-01T00:00:00Z",
    balance: { as_of: "2026-07-20", amount_minor: 10_000 },
    balance_in_base: { amount_minor: 900_000, currency: "RUB", rate_on: "2026-07-19" },
    ...overrides,
  };
}

// Renders AccountDetailPage under the route id it reads its params from
// ("/app/accounts/$accountId", see router.tsx), inside the screen-currency
// provider AppLayout normally supplies.
function renderPage(session: SessionInfo = makeSession()) {
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
  const detailRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/accounts/$accountId",
    component: AccountDetailPage,
  });
  const listRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/accounts",
    component: () => null,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([layoutRoute.addChildren([detailRoute, listRoute])]),
    history: createMemoryHistory({ initialEntries: ["/accounts/acc-1"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

function storeMode(mode: "native" | "base") {
  const { result, unmount } = renderHook(() => useDisplayCurrency());
  act(() => result.current.setMode(mode));
  unmount();
}

describe("AccountDetailPage", () => {
  beforeEach(() => {
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": { body: { positions: [] } },
      "/operations": { body: [] },
      "/api/v1/instruments": { body: { instruments: [] } },
      // The space-wide summary is deliberately broken in every test here:
      // this page must not depend on it at all.
      "/api/v1/summary": { status: 500, body: { error: "internal error" } },
    });
  });

  afterEach(() => {
    storeMode("native");
    window.localStorage.clear();
  });

  it("still shows the account when the space-wide summary endpoint fails", async () => {
    // /summary is one shared, space-wide total; this page shows one account.
    // Letting the former's outage blank out the latter — account, positions
    // and journal alike — costs the user everything for a number that isn't
    // even on this screen.
    renderPage();

    expect(await screen.findByText("Брокерский")).toBeInTheDocument();
    expect(screen.queryByText("Что-то пошло не так")).not.toBeInTheDocument();
    expect(await screen.findByTestId("account-detail-balance")).toBeInTheDocument();
  });

  it("converts the balance using the base currency from the session, not from the summary", async () => {
    // The base currency this page needs is already in the session, so a
    // broken /summary must not stop "base" mode working either.
    storeMode("base");
    renderPage();

    const balance = await screen.findByTestId("account-detail-balance");
    expect(balance.textContent).toMatch(/₽/);
    expect(balance).toHaveAttribute("title", "Пересчитано по курсу на 19.07.2026");
  });
});
