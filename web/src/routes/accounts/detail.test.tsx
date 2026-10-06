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
import {
  ScreenCurrencyCountProvider,
  useHasMultipleScreenCurrencies,
} from "@/lib/screen-currencies";
import { useDisplayCurrency } from "@/lib/display-currency";
import type { SessionInfo } from "@/api/session";
import type { AccountWithBalance } from "@/api/accounts";

// The API client captures globalThis.fetch on first import, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Serves the given endpoints by path suffix and 404s the rest. A substring
// match would serve the account body for its positions request.
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
    full_valuation: "nav_and_foreign",
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

function makeAccount(
  overrides: Partial<AccountWithBalance> = {},
): AccountWithBalance {
  return {
    id: "acc-1",
    name: "Брокерский",
    type: "brokerage",
    currency: "USD",
    institution: "Broker Co",
    status: "active",
    created_at: "2026-01-01T00:00:00Z",
    valued_by_balance: false,
    counted_by: "balance",
    balance: { as_of: "2026-07-20", amount_minor: 10_000 },
    balance_in_base: {
      amount_minor: 900_000,
      currency: "RUB",
      rate_on: "2026-07-19",
    },
    ...overrides,
  };
}

// NBSP-insensitive compare.
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

// Untyped on purpose: these bodies stand in for the server's JSON and may
// carry fields the generated type lacks.
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
    // Always sent; empty here, nothing was paid.
    income_by_currency: [],
    fees_minor: 0,
    currency: "USD",
    has_undated_lots: false,
    has_undated_realizations: false,
    ...overrides,
  };
}

// The account's realized total, both forms; untyped like makePosition.
function makeRealizedTotal(overrides: Record<string, unknown> = {}) {
  return {
    by_currency: [{ currency: "USD", realized_pnl_minor: 0 }],
    base_currency: "RUB",
    in_base: 0,
    in_base_gap: null,
    // Always sent; empty here, nothing was withheld.
    tax_withheld_by_currency: [],
    ...overrides,
  };
}

// One journal row; untyped like makePosition.
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
    valued_by_balance: false,
    counted_by: "balance",
    has_undated_lots: false,
    assembled_from_lots: false,
    in_base: null,
    ...overrides,
  };
}

// A country whose rules differ in two ways; shared so every test looks
// for the same statement.
const britain: SessionInfo["cost_basis_rules"] = {
  country: "GB",
  method: "average",
  perimeter: "owner",
  supported: false,
  notices: ["method_mismatch", "perimeter_mismatch"],
};

// A country whose rule is the queue, so nothing is said. The session and
// the positions response publish the rules separately; a test about the
// source gives one britain and the other this.
const russia: SessionInfo["cost_basis_rules"] = {
  country: "RU",
  method: "fifo",
  perimeter: "account",
  supported: true,
  notices: [],
};

// One position for the cost-basis statement to sit next to;
// realized_total describes the whole list.
function makePositionsBody(
  rules: SessionInfo["cost_basis_rules"],
  positions: unknown[] = [makePosition()],
  realizedTotal: unknown = makeRealizedTotal(),
  accountTotal: unknown = makeAccountTotal(),
) {
  return {
    positions,
    // Always sent; empty by default.
    cash: [],
    cost_basis_rules: rules,
    realized_total: realizedTotal,
    account_total: accountTotal,
  };
}

// The account's headline figure; always sent.
function makeAccountTotal(overrides: Record<string, unknown> = {}) {
  return {
    by_currency: [{ currency: "USD", amount_minor: 0 }],
    base_currency: "RUB",
    in_base: 0,
    in_base_gap: null,
    zero_valued_positions: 0,
    zero_valued_cost_by_currency: [],
    unknown_cost_positions: 0,
    ...overrides,
  };
}

// Stands in for the header toggle, mounted beside the page as AppLayout
// mounts the real one: visible only for more than one currency.
function ToggleProbe() {
  const visible = useHasMultipleScreenCurrencies();
  return <div data-testid="toggle">{visible ? "visible" : "hidden"}</div>;
}

// Under the route id the page reads params from, inside the
// screen-currency provider AppLayout supplies.
function renderPage(session: SessionInfo = makeSession()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  const rootRoute = createRootRoute();
  const layoutRoute = createRoute({
    getParentRoute: () => rootRoute,
    id: "app",
    component: () => (
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
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
    routeTree: rootRoute.addChildren([
      layoutRoute.addChildren([detailRoute, listRoute]),
    ]),
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
        // No positions: by_currency is empty and in_base is plain zero.
        body: makePositionsBody(
          makeSession().cost_basis_rules,
          [],
          makeRealizedTotal({
            by_currency: [],
          }),
        ),
      },
      "/operations": { body: { operations: [], has_more: false } },
      "/api/v1/instruments": { body: { instruments: [], has_more: false } },
      // The space-wide summary is broken in every test: this page must not
      // depend on it.
      "/api/v1/summary": { status: 500, body: { error: "internal error" } },
    });
  });

  afterEach(() => {
    storeMode("native");
    window.localStorage.clear();
  });

  it("still shows the account when the space-wide summary endpoint fails", async () => {
    // A space-wide total's outage must not blank out one account's page.
    renderPage();

    expect(await screen.findByText("Брокерский")).toBeInTheDocument();
    expect(screen.queryByText("Что-то пошло не так")).not.toBeInTheDocument();
    // The account's own figures are here, from the account's own request.
    expect(await screen.findByTestId("account-total")).toBeInTheDocument();
  });

  it("says next to the positions that the figures are not this country's cost basis", async () => {
    // The statement that these figures are not the country's rules, beside
    // the positions it qualifies.
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
      "/operations": { body: { operations: [], has_more: false } },
      "/api/v1/instruments": { body: { instruments: [], has_more: false } },
    });

    renderPage();

    // Both divergences are named: method and perimeter.
    expect(
      await screen.findByText(/не самая ранняя покупка/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/сразу по всем счетам владельца/),
    ).toBeInTheDocument();
    // The country is named, so "в этой стране" has a referent.
    const notice = screen.getByTestId("cost-basis-notice");
    expect(notice.textContent).toContain("Великобритания");
    // The mechanics are in the tooltip, translated, not in the text.
    expect(notice.getAttribute("title")).toContain("стоимость усредняется");
    expect(notice.getAttribute("title")).toContain(
      "сразу по всем счетам владельца",
    );
    expect(notice.textContent).not.toContain("average");
    expect(notice.textContent).not.toContain("стоимость усредняется");
  });

  it("leads from a paper's name in the journal to the paper's own page", async () => {
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": { body: makePositionsBody(makeSession().cost_basis_rules, []) },
      "/operations": {
        body: { operations: [makeOperation({ type: "buy", instrument_id: "instr-9", quantity: "1", price: "10", amount_minor: -1000 })], has_more: false },
      },
      "/api/v1/instruments": {
        body: {
          instruments: [{ id: "instr-9", type: "share", name: "Журнальная", ticker: "JRN", isin: "", figi: "", currency: "USD", frozen: false }],
          has_more: false,
        },
      },
    });
    renderPage();
    expect(await screen.findByRole("link", { name: "Журнальная" })).toHaveAttribute("href", "/instruments/instr-9");
  });

  it("leads from a paper's name to the paper's own page", async () => {
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": { body: makePositionsBody(makeSession().cost_basis_rules) },
      "/operations": { body: { operations: [], has_more: false } },
      "/api/v1/instruments": { body: { instruments: [], has_more: false } },
    });
    renderPage();
    expect(await screen.findByRole("link", { name: "Test Corp" })).toHaveAttribute(
      "href",
      "/instruments/instr-1",
    );
  });

  it("shows in the header what this account's closed deals have locked in", async () => {
    // The server's total: the positions below deliberately do not add up to
    // it, so summing them would print 125,00 $ and fail.
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": {
        body: makePositionsBody(
          makeSession().cost_basis_rules,
          [
            makePosition({ realized_pnl_minor: 10_000 }),
            makePosition({
              instrument_id: "instr-2",
              realized_pnl_minor: 2_500,
            }),
          ],
          makeRealizedTotal({
            by_currency: [{ currency: "USD", realized_pnl_minor: 12_600 }],
          }),
        ),
      },
      "/operations": { body: { operations: [], has_more: false } },
      "/api/v1/instruments": { body: { instruments: [], has_more: false } },
    });

    renderPage();

    expect(
      await screen.findByText("Реализованная прибыль"),
    ).toBeInTheDocument();
    const amounts = await screen.findByTestId("realized-total-amounts");
    expect(norm(amounts.textContent ?? "")).toContain("126,00 $");
  });

  it("follows the display-currency toggle into the base currency", async () => {
    // The header line obeys the same toggle.
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
      "/operations": { body: { operations: [], has_more: false } },
      "/api/v1/instruments": { body: { instruments: [], has_more: false } },
    });

    renderPage();

    const amounts = await screen.findByTestId("realized-total-amounts");
    expect(norm(amounts.textContent ?? "")).toContain("9 000,00 ₽");
  });

  it("says nothing about locked-in results on an account with no positions", async () => {
    // No "0,00" over an empty account.
    renderPage();

    expect(
      await screen.findByText("На этом счете пока нет позиций"),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("realized-total")).not.toBeInTheDocument();
  });

  it("takes the journal's cost basis caveat from the session, not from the positions response", async () => {
    // #61: a transfer's amount is a queue-picked cost basis. The journal does
    // not carry the rules, so the screen takes them from the session. The two
    // publishers disagree here on purpose, so the test asserts the source; no
    // positions, so nothing leaks down the page.
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": {
        body: makePositionsBody(
          russia,
          [],
          makeRealizedTotal({ by_currency: [] }),
        ),
      },
      "/operations": {
        body: {
          operations: [
            makeOperation({
              id: "op-transfer",
              type: "transfer_in",
              // What makes the row a cost basis; a property of the operation, not of
              // in_base.
              assembled_from_lots: true,
              in_base: {
                amount_minor: 900_000,
                fee_minor: 0,
                currency: "RUB",
                rate_on: "2026-06-15",
                dated_on: "2026-06-15",
              },
            }),
          ],
          has_more: false,
        },
      },
      "/api/v1/instruments": { body: { instruments: [], has_more: false } },
    });

    renderPage(makeSession({ tax_residency: "GB", cost_basis_rules: britain }));

    // The caveat sits ON the figure it describes, not over the table.
    const caveat = await screen.findByTestId("operation-amount-caveat");
    const title = caveat.getAttribute("title") ?? "";
    expect(title).toContain("Великобритания");
    // Both divergences, as beside the positions.
    expect(title).toContain("не самая ранняя покупка");
    expect(title).toContain("сразу по всем счетам владельца");
    // And what the figure is, since a cell tooltip says it alone.
    expect(title).toContain("стоимость бумаг");
  });

  it("says nothing about the rules over a journal row that publishes no cost basis", async () => {
    // A deposit was not picked by any queue; a caveat on it is noise.
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": {
        body: makePositionsBody(
          russia,
          [],
          makeRealizedTotal({ by_currency: [] }),
        ),
      },
      "/operations": {
        body: { operations: [makeOperation()], has_more: false },
      },
      "/api/v1/instruments": { body: { instruments: [], has_more: false } },
    });

    renderPage(makeSession({ tax_residency: "GB", cost_basis_rules: britain }));

    expect(await screen.findByText("пополнение")).toBeInTheDocument();
    expect(
      screen.queryByTestId("operation-amount-caveat"),
    ).not.toBeInTheDocument();
    expect(screen.queryByTestId("cost-basis-notice")).not.toBeInTheDocument();
  });

  it("states the cost basis rules once on a screen that shows both positions and a transfer", async () => {
    // No second copy of the positions paragraph over the journal.
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": { body: makePositionsBody(britain) },
      "/operations": {
        body: {
          operations: [
            makeOperation({
              id: "op-transfer",
              type: "transfer_out",
              // A parcel with a stored breakdown, so a rule chose this amount and the
              // caveat is true of it (#81).
              assembled_from_lots: true,
            }),
          ],
          has_more: false,
        },
      },
      "/api/v1/instruments": { body: { instruments: [], has_more: false } },
    });

    renderPage(makeSession({ tax_residency: "GB", cost_basis_rules: britain }));

    // Exactly one block of prose about the rules, and it is the positions'.
    expect(await screen.findByText("Test Corp")).toBeInTheDocument();
    expect(screen.getAllByTestId("cost-basis-notice")).toHaveLength(1);
    // The journal's own figure is still qualified — on the figure itself.
    expect(screen.getByTestId("operation-amount-caveat")).toBeInTheDocument();
  });

  it("says nothing about the rules when the computation is the country's own", async () => {
    // Russia's rule is what the engine computes: no banner.
    serve({
      "/api/v1/accounts": { body: [makeAccount()] },
      "/positions": { body: makePositionsBody(makeSession().cost_basis_rules) },
      "/operations": { body: { operations: [], has_more: false } },
      "/api/v1/instruments": { body: { instruments: [], has_more: false } },
    });

    renderPage();

    expect(await screen.findByText("Test Corp")).toBeInTheDocument();
    expect(screen.queryByTestId("cost-basis-notice")).not.toBeInTheDocument();
  });

  // #48: two currency reporters on the real screen, the page and the
  // journal. These pin that each reporter's set reaches the mode the other
  // half is drawn in; deleting either turns one red.
  //
  // They do not pin the merge rule: a replacing counter keeps the last
  // speaker, which here is the two-currency fetch on both fixtures, and no
  // lever on this screen can make the order deterministic. Replacement is
  // caught by the synthetic reporters in lib/screen-currencies.test.tsx.
  describe("two currency reporters on one screen", () => {
    it("keeps the journal's currencies when the page reports only the base one", async () => {
      // A rouble account, rouble base, no positions; only the journal knows
      // about its dollar deposit.
      serve({
        "/api/v1/accounts": {
          body: [
            makeAccount({
              currency: "RUB",
              balance: { as_of: "2026-07-20", amount_minor: 500_000 },
              balance_in_base: undefined,
            }),
          ],
        },
        "/positions": {
          body: makePositionsBody(
            russia,
            [],
            makeRealizedTotal({ by_currency: [] }),
          ),
        },
        "/operations": {
          body: {
            operations: [
              makeOperation({
                currency: "USD",
                amount_minor: 100_00,
                in_base: {
                  amount_minor: 785_000,
                  fee_minor: 0,
                  currency: "RUB",
                  rate_on: "2026-07-20",
                  dated_on: "2026-07-20",
                },
              }),
            ],
            has_more: false,
          },
        },
        "/api/v1/instruments": { body: { instruments: [], has_more: false } },
      });
      storeMode("base");

      renderPage();

      await screen.findByTestId("operation-amount");
      expect(screen.getByTestId("toggle")).toHaveTextContent("visible");
      // The mode is handed down too: the journal's row gets converted.
      expect(
        norm(screen.getByTestId("operation-amount").textContent ?? ""),
      ).toContain("7 850,00 ₽");
    });

    it("keeps the page's currencies when the journal reports only the base one", async () => {
      // The mirror: a dollar account, a rouble journal. The narrower report must
      // not take the account's dollars off the count.
      serve({
        "/api/v1/accounts": { body: [makeAccount()] },
        "/positions": {
          body: makePositionsBody(
            russia,
            [],
            makeRealizedTotal({ by_currency: [] }),
          ),
        },
        "/operations": {
          body: {
            operations: [
              makeOperation({ currency: "RUB", amount_minor: 100_000 }),
            ],
            has_more: false,
          },
        },
        "/api/v1/instruments": { body: { instruments: [], has_more: false } },
      });
      storeMode("base");

      renderPage();

      await screen.findByTestId("operation-amount");
      expect(screen.getByTestId("toggle")).toHaveTextContent("visible");
    });
  });
});
