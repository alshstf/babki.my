import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
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

// The API client captures globalThis.fetch on first import, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Serves the given endpoints by URL substring and 404s the rest.
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

function makeSession(role: SessionInfo["role"] = "owner"): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role,
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    full_valuation: "nav_and_foreign",
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
    valued_by_balance: false,
    counted_by: "balance",
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
    journal: {
      accounts: 0,
      differing: 0,
      differing_difference_minor: 0,
      pinned_to_balance: 0,
      unpriced_positions: 0,
      not_traded_positions: 0,
      full_difference_minor: 0,
    },
    ...overrides,
  };
}

// As AppLayout renders it: inside the screen-currency provider and a
// router (rows link to the detail route).
function renderPage(role: SessionInfo["role"] = "owner") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], makeSession(role));
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
  return {
    ...render(
      <QueryClientProvider client={qc}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    ),
    qc,
  };
}

// Writes the stored choice as the header toggle does; the store is
// module-level.
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
    // "Base" chosen on a multi-currency screen, then a single-currency one
    // hides the toggle: the breakdown cards must not vanish with no way back.
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

// #95: the confirmation printed the server's English log prose.
describe("AccountsPage — an archive the server refused", () => {
  it("says it in Russian and does not repeat the server's own words", async () => {
    // Method-aware: the archive DELETE goes to the list's path, so a
    // path-only mock would answer it with a 200.
    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = input instanceof Request ? input.method : (init?.method ?? "GET");
      const json = (status: number, body: unknown) =>
        Promise.resolve(
          new Response(JSON.stringify(body), {
            status,
            headers: { "Content-Type": "application/json" },
          }),
        );
      if (method === "DELETE") return json(409, { error: "account has operations" });
      if (url.includes("/api/v1/summary")) return json(200, makeSummary());
      if (url.includes("/api/v1/accounts")) return json(200, [makeAccount()]);
      return json(404, null);
    });
    renderPage();

    // Radix's menu opens on pointerdown, which jsdom lacks; Enter opens it too.
    fireEvent.keyDown(await screen.findByRole("button", { name: "Действия" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Архивировать" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Архивировать" }));

    expect(await within(dialog).findByText("Не удалось архивировать счет")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("account has operations");
  });

  // #21: Cancel is a plain button and Radix calls onOpenChange only for its
  // own dismiss triggers, so the refusal stayed for the next account.
  it("opens clean afterwards instead of carrying the refusal to the next account", async () => {
    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = input instanceof Request ? input.method : (init?.method ?? "GET");
      const json = (status: number, body: unknown) =>
        Promise.resolve(
          new Response(JSON.stringify(body), {
            status,
            headers: { "Content-Type": "application/json" },
          }),
        );
      if (method === "DELETE") return json(409, { error: "account has operations" });
      if (url.includes("/api/v1/summary")) return json(200, makeSummary());
      if (url.includes("/api/v1/accounts")) return json(200, [makeAccount()]);
      return json(404, null);
    });
    renderPage();

    const openArchive = async () => {
      fireEvent.keyDown(await screen.findByRole("button", { name: "Действия" }), { key: "Enter" });
      fireEvent.click(await screen.findByRole("menuitem", { name: "Архивировать" }));
      return screen.findByRole("dialog");
    };

    const dialog = await openArchive();
    fireEvent.click(within(dialog).getByRole("button", { name: "Архивировать" }));
    expect(await within(dialog).findByText("Не удалось архивировать счет")).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("button", { name: "Отмена" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    const reopened = await openArchive();
    expect(within(reopened).queryByText("Не удалось архивировать счет")).toBeNull();
  });
});

describe("AccountsPage — bringing an account back from the archive", () => {
  // An archived account takes no entries, so its menu offers the way back:
  // a PATCH of its status.
  it("offers «Вернуть из архива» on an archived row and sends the account back as active", async () => {
    const sent: { method: string; url: string; body: unknown }[] = [];
    fetchMock.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = input instanceof Request ? input.method : (init?.method ?? "GET");
      const text = input instanceof Request ? await input.text() : String(init?.body ?? "");
      const json = (body: unknown) =>
        new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
      if (method !== "GET") {
        sent.push({ method, url, body: text ? JSON.parse(text) : null });
        return json(makeAccount());
      }
      if (url.includes("/api/v1/summary")) return json(makeSummary());
      if (url.includes("/api/v1/accounts")) return json([makeAccount({ status: "archived" })]);
      return new Response("null", { status: 404 });
    });
    renderPage();

    fireEvent.keyDown(await screen.findByRole("button", { name: "Действия" }), { key: "Enter" });
    expect(screen.queryByRole("menuitem", { name: "Архивировать" })).toBeNull();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Вернуть из архива" }));

    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].method).toBe("PATCH");
    expect(sent[0].url).toContain("/api/v1/accounts/acc-1");
    expect(sent[0].body).toEqual({ status: "active" });
  });

  it("offers an active row «Архивировать» and not the way back", async () => {
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/api/v1/summary": { body: makeSummary() },
    });
    renderPage();
    fireEvent.keyDown(await screen.findByRole("button", { name: "Действия" }), { key: "Enter" });
    expect(await screen.findByRole("menuitem", { name: "Архивировать" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Вернуть из архива" })).toBeNull();
  });
});

// Every button by accessible name, so the tests compare two roles'
// screens: a new control shows up in the diff by itself, where a
// hand-written list would miss it.
function buttonNames(): string[] {
  return screen
    .queryAllByRole("button")
    .map((b) => (b.getAttribute("aria-label") ?? b.textContent ?? "").trim())
    .filter((name) => name !== "")
    .sort();
}

// #14: a viewer may read and change nothing; the server enforces it, and
// the screen must not offer controls that always fail.
describe("AccountsPage — what a viewer may do", () => {
  beforeEach(() => {
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/api/v1/summary": { body: makeSummary() },
    });
  });

  it("offers a viewer exactly the owner's screen minus its write controls", async () => {
    renderPage("owner");
    await screen.findByTestId("account-balance-acc-1");
    const asOwner = buttonNames();

    cleanup();

    renderPage("viewer");
    await screen.findByTestId("account-balance-acc-1");
    const asViewer = buttonNames();

    // Written out so a newly gated control is added deliberately.
    expect(asOwner.filter((name) => !asViewer.includes(name))).toEqual([
      "Действия",
      "Добавить счет",
    ]);
    // Nothing the other way: a mis-negated condition shows here.
    expect(asViewer.filter((name) => !asOwner.includes(name))).toEqual([]);
    // The screen is still a screen: the figures a viewer came for are there.
    expect(screen.getByText("Наличные")).toBeInTheDocument();
  });

  it("gives an editor the write controls a viewer does not get", async () => {
    // Otherwise a page showing write controls to nobody would pass.
    renderPage("editor");
    await screen.findByTestId("account-balance-acc-1");
    expect(screen.getByRole("button", { name: "Добавить счет" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Действия" })).toBeInTheDocument();
  });
});

// #200: a failed background refresh must not replace the data on screen
// with «Что-то пошло не так».
describe("AccountsPage — data already on screen survives a failed refresh", () => {
  it("keeps the list and says the refresh failed", async () => {
    serve({
      "/api/v1/accounts": { body: [makeAccount({ name: "Наличные" })] },
      "/api/v1/summary": { body: makeSummary() },
    });
    const { qc } = renderPage();
    expect(await screen.findByText("Наличные")).toBeInTheDocument();

    serve({
      "/api/v1/accounts": { status: 500, body: { error: "internal error" } },
      "/api/v1/summary": { body: makeSummary() },
    });
    await qc.refetchQueries({ queryKey: ["accounts"] });

    await waitFor(() => expect(screen.getByTestId("refresh-failed")).toBeInTheDocument());
    expect(screen.getByText("Наличные")).toBeInTheDocument();
    expect(screen.queryByText("Что-то пошло не так")).not.toBeInTheDocument();
  });

  it("says there is no connection, not that there are no accounts", async () => {
    // The browser reports no network: the queries are paused, nothing was asked.
    onlineManager.setOnline(false);
    try {
      serve({});
      fetchMock.mockClear();
      renderPage();
      expect(await screen.findByTestId("query-offline")).toBeInTheDocument();
      expect(screen.queryByText(/Пока нет ни одного счета/)).not.toBeInTheDocument();
      expect(fetchMock).not.toHaveBeenCalled();
    } finally {
      onlineManager.setOnline(true);
    }
  });
});
