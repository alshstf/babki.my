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
    tax_residency: "RU",
    cost_basis_rules: {
      country: "RU",
      method: "fifo",
      perimeter: "account",
      supported: true,
      notices: [],
    },
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

// NBSP-insensitive compare: Intl.NumberFormat uses non-breaking spaces
// (same helper as in positions-table.test.tsx).
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

// One position as the wire sends it — not typed as Position on purpose: these
// bodies stand in for the server's JSON, and a test that needs a field the
// generated type doesn't have yet must still be able to send it.
function makePosition({
  instrument_id = "instr-1",
  ...overrides
}: Record<string, unknown> & { instrument_id?: string } = {}) {
  return {
    instrument: {
      id: instrument_id,
      type: "share",
      name: "Test Corp",
      ticker: "TEST",
      isin: "",
      figi: "",
      currency: "USD",
      frozen: false,
    },
    quantity: "10",
    cost_minor: 250_000,
    realized_pnl_minor: 0,
    income_minor: 0,
    fees_minor: 0,
    currency: "USD",
    has_undated_lots: false,
    has_undated_realizations: false,
    ...overrides,
  };
}

// The account's realized total as the server publishes it, both forms at once
// (see RealizedTotal in the API contract). Untyped for the same reason
// makePosition is: these bodies stand in for the server's JSON.
function makeRealizedTotal(overrides: Record<string, unknown> = {}) {
  return {
    by_currency: [{ currency: "USD", realized_pnl_minor: 0 }],
    base_currency: "RUB",
    in_base: 0,
    in_base_gap: null,
    ...overrides,
  };
}

// One journal row as the wire sends it. Untyped for the same reason
// makePosition is: these bodies stand in for the server's JSON.
function makeOperation(overrides: Record<string, unknown> = {}) {
  return {
    id: "op-1",
    account_id: "acc-1",
    instrument_id: null,
    type: "deposit",
    occurred_on: "2026-07-20",
    settled_on: null,
    quantity: null,
    price: null,
    amount_minor: 100_000,
    currency: "USD",
    fee_minor: 0,
    note: "",
    transfer_group_id: null,
    split_ratio: null,
    source: "manual",
    created_at: "2026-07-20T00:00:00Z",
    has_undated_lots: false,
    in_base: null,
    ...overrides,
  };
}

// A country whose rules are not what this application computes, in two
// separate ways at once. Shared by the tests below so the statement they look
// for is the same statement wherever it is shown.
const britain: SessionInfo["cost_basis_rules"] = {
  country: "GB",
  method: "average",
  perimeter: "owner",
  supported: false,
  notices: ["method_mismatch", "perimeter_mismatch"],
};

// One position, enough of one for the table to render a row. The cost basis
// statement below qualifies exactly these figures, so the tests about it need
// a row for it to sit next to. realized_total describes the whole list rather
// than any one row, so it travels alongside them.
function makePositionsBody(
  rules: SessionInfo["cost_basis_rules"],
  positions: unknown[] = [makePosition()],
  realizedTotal: unknown = makeRealizedTotal(),
) {
  return { positions, cost_basis_rules: rules, realized_total: realizedTotal };
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
      "/positions": {
        // An account with no positions: the server's total is empty too —
        // by_currency has no entries and in_base is the plain zero of no
        // deals at all.
        body: makePositionsBody(makeSession().cost_basis_rules, [], makeRealizedTotal({
          by_currency: [],
        })),
      },
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
    expect(balance).toHaveAttribute("title", "Пересчитано по текущему курсу (на 19.07.2026)");
  });

  it("says next to the positions that the figures are not this country's cost basis", async () => {
    // The whole point of the residency work: the response already carries
    // "these numbers are not what your country's rules produce", and until
    // this appears on the screen the statement only exists for whoever reads
    // the JSON. It sits with the positions because that is where the figures
    // it qualifies are shown.
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": {
        body: makePositionsBody({
          country: "GB",
          method: "average",
          perimeter: "owner",
          supported: false,
          notices: ["method_mismatch", "perimeter_mismatch"],
        }),
      },
      "/operations": { body: [] },
      "/api/v1/instruments": { body: { instruments: [] } },
    });

    renderPage();

    // Both divergences are named, not just the first: Britain differs in the
    // method AND in the perimeter, and reporting one hides the other.
    expect(
      await screen.findByText(/не самая ранняя покупка/),
    ).toBeInTheDocument();
    expect(screen.getByText(/сразу по всем счетам владельца/)).toBeInTheDocument();
    // The country is named, so "в этой стране" has a referent on a screen
    // that never mentions the residency otherwise.
    const notice = screen.getByTestId("cost-basis-notice");
    expect(notice.textContent).toContain("Великобритания");
    // The mechanics — what this country's rule actually is — belong in the
    // tooltip, not in the text of a screen full of figures (the owner's
    // standing rule about technical detail being visual noise). Translated
    // there too: "average"/"owner" would be the wire format talking to a
    // person.
    expect(notice.getAttribute("title")).toContain("стоимость усредняется");
    expect(notice.getAttribute("title")).toContain("сразу по всем счетам владельца");
    expect(notice.textContent).not.toContain("average");
    expect(notice.textContent).not.toContain("стоимость усредняется");
  });

  it("shows in the header what this account's closed deals have locked in", async () => {
    // The figure arrives added up, from the response that carries the rows it
    // stands over. This screen renders the server's total and computes none of
    // its own — the positions below deliberately do NOT add up to it, so a
    // client that went back to summing them would print 125,00 $ and fail.
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": {
        body: makePositionsBody(
          makeSession().cost_basis_rules,
          [
            makePosition({ realized_pnl_minor: 10_000 }),
            makePosition({ instrument_id: "instr-2", realized_pnl_minor: 2_500 }),
          ],
          makeRealizedTotal({ by_currency: [{ currency: "USD", realized_pnl_minor: 12_600 }] }),
        ),
      },
      "/operations": { body: [] },
      "/api/v1/instruments": { body: { instruments: [] } },
    });

    renderPage();

    expect(await screen.findByText("Зафиксировано")).toBeInTheDocument();
    const amounts = await screen.findByTestId("realized-total-amounts");
    expect(norm(amounts.textContent ?? "")).toContain("126,00 $");
  });

  it("follows the display-currency toggle into the base currency", async () => {
    // The header line is not a second, independent opinion about which
    // currency to speak: it obeys the same toggle every other figure does.
    storeMode("base");
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": {
        body: makePositionsBody(
          makeSession().cost_basis_rules,
          [makePosition({ realized_pnl_minor: 10_000 })],
          makeRealizedTotal({
            by_currency: [{ currency: "USD", realized_pnl_minor: 10_000 }],
            in_base: 900_000,
          }),
        ),
      },
      "/operations": { body: [] },
      "/api/v1/instruments": { body: { instruments: [] } },
    });

    renderPage();

    const amounts = await screen.findByTestId("realized-total-amounts");
    expect(norm(amounts.textContent ?? "")).toContain("9 000,00 ₽");
  });

  it("says nothing about locked-in results on an account with no positions", async () => {
    // The default body served in beforeEach has no positions at all, and a
    // "0,00" over an empty account answers a question nobody asked.
    renderPage();

    expect(await screen.findByText("На этом счете пока нет позиций")).toBeInTheDocument();
    expect(screen.queryByTestId("realized-total")).not.toBeInTheDocument();
  });

  it("says next to the journal that a transferred amount is not this country's cost basis", async () => {
    // Issue #61. A transfer's amount is the cost basis of the shares it moved,
    // picked by the same earliest-purchases-first queue the positions screen
    // uses — so the statement "that queue is not your country's" describes it
    // too. The journal response does not carry the statement (one truth, one
    // publisher — see SessionInfo.cost_basis_rules in the API contract), so the
    // screen takes it from the session it has already loaded. Until it did, a
    // client that faithfully read the caveat in both places the server offers
    // it still showed this figure with nothing said about it.
    //
    // The account deliberately has NO positions: the notice that appears can
    // then only be the journal's own, not the positions table's leaking down
    // the page.
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": { body: makePositionsBody(britain, [], makeRealizedTotal({ by_currency: [] })) },
      "/operations": { body: [makeOperation({ id: "op-transfer", type: "transfer_in" })] },
      "/api/v1/instruments": { body: [] },
    });

    renderPage(makeSession({ tax_residency: "GB", cost_basis_rules: britain }));

    // The journal really is the only thing on screen that could be qualified.
    expect(await screen.findByText("На этом счете пока нет позиций")).toBeInTheDocument();
    const notice = await screen.findByTestId("cost-basis-notice");
    expect(notice.textContent).toContain("Великобритания");
    // Both divergences, exactly as beside the positions: reporting one hides
    // the other.
    expect(screen.getByText(/не самая ранняя покупка/)).toBeInTheDocument();
    expect(screen.getByText(/сразу по всем счетам владельца/)).toBeInTheDocument();
  });

  it("says nothing about the rules over a journal that publishes no cost basis", async () => {
    // A deposit's amount is money that moved on the day the row is dated; no
    // queue picked it and no cost basis rule has any part in it. A caveat over
    // such a table is a caveat about nothing, and noise is what makes a real
    // warning invisible — the same reason the positions notice stays away from
    // an empty table.
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": { body: makePositionsBody(britain, [], makeRealizedTotal({ by_currency: [] })) },
      "/operations": { body: [makeOperation()] },
      "/api/v1/instruments": { body: [] },
    });

    renderPage(makeSession({ tax_residency: "GB", cost_basis_rules: britain }));

    expect(await screen.findByText("пополнение")).toBeInTheDocument();
    expect(screen.queryByTestId("cost-basis-notice")).not.toBeInTheDocument();
  });

  it("says nothing about the rules when the computation is the country's own", async () => {
    // Russia's rule is exactly what the engine computes. A banner on every
    // visit for a reader with nothing to be warned about is noise, and noise
    // is what makes a real warning invisible.
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": { body: makePositionsBody(makeSession().cost_basis_rules) },
      "/operations": { body: [] },
      "/api/v1/instruments": { body: { instruments: [] } },
    });

    renderPage();

    expect(await screen.findByText("Test Corp")).toBeInTheDocument();
    expect(screen.queryByTestId("cost-basis-notice")).not.toBeInTheDocument();
  });
});
