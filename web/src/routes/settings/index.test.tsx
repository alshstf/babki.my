import { describe, expect, it, vi, beforeEach } from "vitest";
import { cleanup, render, screen, fireEvent, waitFor, within } from "@testing-library/react";
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
import { SettingsPage } from "./index";
import type { SessionInfo } from "@/api/session";
import type { CostBasisRules } from "@/api/tax-residencies";
import type { TinvestConnection } from "@/api/connections";
import { AccountDialog } from "@/routes/accounts/account-dialog";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// jsdom lacks scrollIntoView, which Radix Select calls on open.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

// Serves the given endpoints by path suffix and 404s the rest; this
// screen reads both the session and the country list.
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

// The PATCH bodies sent, in order. openapi-fetch passes a Request, which
// is cloned before its one-shot body is read.
async function patchBodies(): Promise<Record<string, unknown>[]> {
  const calls = fetchMock.mock.calls.filter(([input, init]) => {
    const url = input instanceof Request ? input.url : String(input);
    const method =
      input instanceof Request ? input.method : (init as RequestInit | undefined)?.method;
    return url.endsWith("/api/v1/space") && method === "PATCH";
  });
  return Promise.all(
    calls.map(async ([input, init]) => {
      const raw =
        input instanceof Request
          ? await input.clone().text()
          : String((init as RequestInit).body);
      return JSON.parse(raw) as Record<string, unknown>;
    }),
  );
}

const RU_RULES: CostBasisRules = {
  country: "RU",
  method: "fifo",
  perimeter: "account",
  supported: true,
  notices: [],
};

// Britain diverges in both ways, proving every divergence is shown.
const GB_RULES: CostBasisRules = {
  country: "GB",
  method: "average",
  perimeter: "owner",
  supported: false,
  notices: ["method_mismatch", "perimeter_mismatch"],
};

const DE_RULES: CostBasisRules = {
  country: "DE",
  method: "fifo",
  perimeter: "account",
  supported: true,
  notices: [],
};

function makeSession(overrides: Partial<SessionInfo> = {}): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role: "owner",
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    full_valuation: "nav_and_foreign",
    tax_residency: "RU",
    cost_basis_rules: RU_RULES,
    ...overrides,
  };
}

// The session is seeded into the query cache; the country list is served
// over the network, since "the list comes from the server" is under test.
// The router's stub routes stand in for the connection screens this page
// links to.
function wrap(session: SessionInfo) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const settingsRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/settings",
    component: SettingsPage,
  });
  const connectRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/settings/connections/new",
    component: () => null,
  });
  const connectionDetailRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/settings/connections/$connectionId",
    component: () => null,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([settingsRoute, connectRoute, connectionDetailRoute]),
    history: createMemoryHistory({ initialEntries: ["/settings"] }),
  });
  return {
    qc,
    ...render(
      <QueryClientProvider client={qc}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    ),
  };
}

const currencySelect = () => screen.getByRole("combobox", { name: "Базовая валюта" });
const countrySelect = () =>
  screen.getByRole("combobox", { name: "Страна налогового резидентства" });
const saveButton = () => screen.getByRole("button", { name: "Сохранить" });

describe("SettingsPage", () => {
  beforeEach(() => {
    fetchMock.mockReset();
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
      "/api/v1/tinvest/connections": { body: { connections: [] } },
    });
  });

  it("shows the base currency form for an owner with Save disabled until something changes", async () => {
    wrap(makeSession({ base_currency: "RUB" }));

    expect(await screen.findByText("Базовая валюта")).toBeInTheDocument();
    expect(currencySelect()).toBeInTheDocument();
    expect(saveButton()).toBeDisabled();
  });

  it("offers the whole space as a download", async () => {
    wrap(makeSession());
    const link = await screen.findByTestId("settings-export-link");
    expect(link).toHaveAttribute("href", "/api/v1/export");
    expect(link).toHaveAttribute("download");
  });

  it("enables Save once a different currency is selected", async () => {
    wrap(makeSession({ base_currency: "RUB" }));

    fireEvent.click(await screen.findByRole("combobox", { name: "Базовая валюта" }));
    fireEvent.click(screen.getByText("USD"));

    expect(saveButton()).toBeEnabled();
  });

  it("invalidates cached account balances after the base currency changes", async () => {
    // Account rows carry balance_in_base in the old currency, so the
    // cached list is marked stale with the summary.
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
      "/api/v1/tinvest/connections": { body: { connections: [] } },
      "/api/v1/space": { body: makeSession({ base_currency: "USD" }) },
    });

    const { qc } = wrap(makeSession({ base_currency: "RUB" }));
    qc.setQueryData(["accounts"], []);
    qc.setQueryData(["summary"], null);

    fireEvent.click(await screen.findByRole("combobox", { name: "Базовая валюта" }));
    fireEvent.click(screen.getByText("USD"));
    fireEvent.click(saveButton());

    await waitFor(() => {
      expect(qc.getQueryState(["accounts"])?.isInvalidated).toBe(true);
    });
    expect(qc.getQueryState(["summary"])?.isInvalidated).toBe(true);
  });

  it("invalidates cached operations and positions after the base currency changes", async () => {
    // The account screen's journal and positions caches hold old-currency
    // figures; a refetch landing between the switch and a re-render would
    // print them under the new symbol.
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
      "/api/v1/tinvest/connections": { body: { connections: [] } },
      "/api/v1/space": { body: makeSession({ base_currency: "USD" }) },
    });

    const { qc } = wrap(makeSession({ base_currency: "RUB" }));
    qc.setQueryData(["operations", "acc-1", 50, 0], []);
    qc.setQueryData(["positions", "acc-1"], []);

    fireEvent.click(await screen.findByRole("combobox", { name: "Базовая валюта" }));
    fireEvent.click(screen.getByText("USD"));
    fireEvent.click(saveButton());

    await waitFor(() => {
      expect(qc.getQueryState(["operations", "acc-1", 50, 0])?.isInvalidated).toBe(true);
    });
    expect(qc.getQueryState(["positions", "acc-1"])?.isInvalidated).toBe(true);
  });

  it("says the settings were saved, and stops saying it once the form changes again", async () => {
    // #33: the only sign a save succeeded; the fields already showed the new
    // values.
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
      "/api/v1/tinvest/connections": { body: { connections: [] } },
      "/api/v1/space": { body: makeSession({ base_currency: "USD" }) },
    });

    wrap(makeSession({ base_currency: "RUB" }));

    fireEvent.click(await screen.findByRole("combobox", { name: "Базовая валюта" }));
    fireEvent.click(screen.getByText("USD"));
    expect(screen.queryByTestId("settings-saved")).not.toBeInTheDocument();

    fireEvent.click(saveButton());

    const saved = await screen.findByTestId("settings-saved");
    expect(saved).toHaveTextContent("Сохранено");
    // A status, not an alert: this confirms what the reader asked for.
    expect(saved).toHaveAttribute("role", "status");

    // Touching a field again makes the confirmation false, so it goes.
    fireEvent.click(screen.getByRole("combobox", { name: "Базовая валюта" }));
    fireEvent.click(screen.getByText("EUR"));

    expect(screen.queryByTestId("settings-saved")).not.toBeInTheDocument();
  });

  it("says nothing about a save that failed", async () => {
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
      "/api/v1/tinvest/connections": { body: { connections: [] } },
      "/api/v1/space": { status: 500, body: { error: "internal error" } },
    });

    wrap(makeSession({ base_currency: "RUB" }));

    fireEvent.click(await screen.findByRole("combobox", { name: "Базовая валюта" }));
    fireEvent.click(screen.getByText("USD"));
    fireEvent.click(saveButton());

    expect(await screen.findByText("Что-то пошло не так")).toBeInTheDocument();
    expect(screen.queryByTestId("settings-saved")).not.toBeInTheDocument();
  });

  // #33: one decision reaches both currency selectors. The rendered options
  // are compared with each other, never with COMMON_CURRENCIES, which would
  // pass with a second copy restored on one screen.
  it("offers the same ready-made currencies the account dialog does", async () => {
    const optionLabels = () =>
      screen.getAllByRole("option").map((option) => option.textContent ?? "");

    wrap(makeSession({ base_currency: "RUB" }));
    fireEvent.click(await screen.findByRole("combobox", { name: "Базовая валюта" }));
    const inSettings = optionLabels();
    cleanup();

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(["session"], makeSession());
    render(
      <QueryClientProvider client={qc}>
        <AccountDialog open onOpenChange={() => {}} />
      </QueryClientProvider>,
    );
    fireEvent.click(await screen.findByRole("combobox", { name: "Валюта" }));
    const inAccountDialog = optionLabels();

    // Both end with the same «Другая…», so the whole offer is compared.
    expect(inSettings).toEqual(inAccountDialog);
    expect(inSettings.at(-1)).toBe("Другая…");
    // Not a tautology of two empty lists.
    expect(inSettings.length).toBeGreaterThan(1);
  });

  it("shows an owner-only message and no form for a non-owner", async () => {
    wrap(makeSession({ role: "editor" }));

    expect(
      await screen.findByText("Настройки доступны только владельцу пространства"),
    ).toBeInTheDocument();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Сохранить" })).not.toBeInTheDocument();
  });

  describe("tax residency", () => {
    it("offers exactly the countries the server sent, by name", async () => {
      // The options come from the response only, so they cannot drift from
      // the server's table.
      wrap(makeSession({ tax_residency: "RU" }));

      await waitFor(() => expect(countrySelect()).toBeEnabled());
      fireEvent.click(countrySelect());

      const options = screen.getAllByRole("option").map((o) => o.textContent);
      expect(options).toEqual(["Великобритания", "Германия", "Россия"]);
    });

    it("states what the selected country means for the figures before it is saved", async () => {
      wrap(makeSession({ tax_residency: "RU" }));

      // A computed country says so: the owner opened this screen to ask.
      expect(
        await screen.findByText(/соответствует правилам этой страны/),
      ).toBeInTheDocument();

      await waitFor(() => expect(countrySelect()).toBeEnabled());
      fireEvent.click(countrySelect());
      fireEvent.click(screen.getByText("Великобритания"));

      // Both of Britain's divergences, from the list the server sent.
      const notice = screen.getByTestId("cost-basis-notice");
      expect(within(notice).getByText(/не самая ранняя покупка/)).toBeInTheDocument();
      expect(within(notice).getByText(/сразу по всем счетам владельца/)).toBeInTheDocument();
      expect(within(notice).queryByText(/соответствует правилам этой страны/)).toBeNull();
    });

    it("names a stored country the list does not offer instead of rendering nothing", async () => {
      // A stored country with no rules (edited outside the form, or later
      // dropped): the server says so rather than guessing. The selector has no
      // matching option, and Radix shows an empty trigger for one.
      const unknownRules: CostBasisRules = {
        country: "FR",
        method: "unknown",
        perimeter: "unknown",
        supported: false,
        notices: ["unknown_country"],
      };
      const stored = makeSession({ tax_residency: "FR", cost_basis_rules: unknownRules });
      serve({
        "/api/v1/auth/me": { body: stored },
        "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
        "/api/v1/tinvest/connections": { body: { connections: [] } },
      });
      wrap(stored);

      await waitFor(() => expect(countrySelect()).toBeEnabled());
      expect(countrySelect().textContent).toContain("Франция");
      expect(countrySelect().textContent).not.toContain("Загрузка");
      // The notice was already right about this case and must stay right.
      expect(
        within(screen.getByTestId("cost-basis-notice")).getByText(/Правил этой страны/),
      ).toBeInTheDocument();
    });

    it("saves the country alone and refreshes what its statement travels with", async () => {
      serve({
        "/api/v1/auth/me": { body: makeSession() },
        "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
        "/api/v1/tinvest/connections": { body: { connections: [] } },
        "/api/v1/space": {
          body: makeSession({ tax_residency: "GB", cost_basis_rules: GB_RULES }),
        },
      });

      const { qc } = wrap(makeSession({ tax_residency: "RU" }));
      qc.setQueryData(["positions", "acc-1"], []);

      await waitFor(() => expect(countrySelect()).toBeEnabled());
      fireEvent.click(countrySelect());
      fireEvent.click(screen.getByText("Великобритания"));
      fireEvent.click(saveButton());

      // cost_basis_rules travels with the positions, so a stale cache would
      // show the old country's statement.
      await waitFor(() => {
        expect(qc.getQueryState(["positions", "acc-1"])?.isInvalidated).toBe(true);
      });
      // Only what changed is sent.
      expect(await patchBodies()).toEqual([{ tax_residency: "GB" }]);
    });
  });

  describe("connections", () => {
    function makeConnection(overrides: Partial<TinvestConnection> = {}): TinvestConnection {
      return {
        id: "conn-1",
        status: "active",
        token_last4: "3456",
        accounts: [],
        reconciles: [],
        ...overrides,
      };
    }

    it("shows an empty state with a way to connect T-Invest", async () => {
      wrap(makeSession());

      expect(
        await screen.findByText("Пока нет ни одного подключения к брокеру"),
      ).toBeInTheDocument();
      const connect = screen.getByRole("link", { name: "Подключить Т-Инвестиции" });
      expect(connect).toHaveAttribute("href", "/settings/connections/new");
    });

    it("lists an existing connection with its status and the last four token characters, linking to its own screen", async () => {
      serve({
        "/api/v1/auth/me": { body: makeSession() },
        "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
        "/api/v1/tinvest/connections": {
          body: {
            connections: [makeConnection({ status: "token_revoked", token_last4: "wxyz" })],
          },
        },
      });

      wrap(makeSession());

      expect(await screen.findByText("Токен ···wxyz")).toBeInTheDocument();
      // The badge names the server's verdict (TinvestConnectionStatus).
      expect(screen.getByText("Нужен новый токен")).toBeInTheDocument();

      const links = screen.getAllByRole("link");
      const row = links.find((l) => l.getAttribute("href") === "/settings/connections/conn-1");
      expect(row).toBeDefined();
      expect(row).toHaveTextContent("Т-Инвестиции");
    });

    it("draws a revoked token as a problem, apart from a connection merely switched off", async () => {
      serve({
        "/api/v1/auth/me": { body: makeSession() },
        "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
        "/api/v1/tinvest/connections": {
          body: {
            connections: [
              makeConnection({ id: "conn-1", status: "active" }),
              makeConnection({ id: "conn-2", status: "token_revoked" }),
              makeConnection({ id: "conn-3", status: "disabled" }),
            ],
          },
        },
      });

      wrap(makeSession());

      const revoked = await screen.findByText("Нужен новый токен");
      const off = screen.getByText("Отключено");
      const active = screen.getByText("Активно");

      // A revoked token waits for the owner; `disabled` waits for nobody. The
      // two must not read alike.
      expect(revoked).toHaveAttribute("data-variant", "destructive");
      expect(off).toHaveAttribute("data-variant", "secondary");
      expect(active).toHaveAttribute("data-variant", "default");
      expect(revoked.getAttribute("data-variant")).not.toBe(off.getAttribute("data-variant"));
    });
  });
});
