import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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

function makeSession(role: SessionInfo["role"] = "owner"): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role,
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

// #95: this confirmation printed whatever the server put in its error body,
// which is English written for a developer reading a log.
describe("AccountsPage — an archive the server refused", () => {
  it("says it in Russian and does not repeat the server's own words", async () => {
    // Method-aware: the archive is a DELETE to the very path the accounts list
    // is read from, so a mock keyed on the path alone would answer it with the
    // list — a 200, i.e. a success — and the dialog under test would never see
    // a refusal at all.
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

    // Radix's menu opens on pointerdown, and jsdom has no PointerEvent to fire;
    // the trigger's own keyboard path opens the same menu.
    fireEvent.keyDown(await screen.findByRole("button", { name: "Действия" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Архивировать" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Архивировать" }));

    expect(await within(dialog).findByText("Не удалось архивировать счет")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("account has operations");
  });

  // #21: the alert lives on the mutation, and Cancel is a plain button that
  // clears archiveTarget — Radix calls onOpenChange only for its OWN dismiss
  // triggers (Escape, overlay, DialogClose), so the reset written there never
  // ran on this path. The next confirmation opened already saying an archive
  // had failed, about an account nobody had tried to archive yet.
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

// buttonNames is every button this screen currently offers, by the name a
// person actually reads off it — the accessible name, so an icon-only control
// is named by its aria-label rather than by an empty string.
//
// It exists so the assertions below can be about the DIFFERENCE between two
// roles' screens instead of about a list of controls typed into a test. A
// hand-written list goes on looking complete the day somebody adds a control it
// does not mention, which is exactly the day the test stops being worth
// anything; a diff cannot, because the new control turns up in it by itself
// (the same reason the T-Invest role tests take their route list from the
// router rather than from a literal — see internal/importer/tinvest).
function buttonNames(): string[] {
  return screen
    .queryAllByRole("button")
    .map((b) => (b.getAttribute("aria-label") ?? b.textContent ?? "").trim())
    .filter((name) => name !== "")
    .sort();
}

// #14: every one of these was verified by hand and by nothing else. A viewer is
// a member who may read the family's money and change none of it, and the
// server enforces that — but a screen offering controls that always fail is its
// own defect, and nothing here noticed when one appeared.
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

    // Written out rather than derived from the page: the point of the literal
    // is that a control gated in a NEW place has to be added to it deliberately,
    // by somebody who then has to say why it belongs there.
    expect(asOwner.filter((name) => !asViewer.includes(name))).toEqual([
      "Действия",
      "Добавить счет",
    ]);
    // And nothing the other way round: a viewer must not be offered a control
    // the owner is not, which is what a mis-negated condition looks like.
    expect(asViewer.filter((name) => !asOwner.includes(name))).toEqual([]);
    // The screen is still a screen: the figures a viewer came for are there.
    expect(screen.getByText("Наличные")).toBeInTheDocument();
  });

  it("gives an editor the write controls a viewer does not get", async () => {
    // Otherwise "a viewer sees no write controls" would also pass on a page
    // that shows them to nobody at all.
    renderPage("editor");
    await screen.findByTestId("account-balance-acc-1");
    expect(screen.getByRole("button", { name: "Добавить счет" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Действия" })).toBeInTheDocument();
  });
});
