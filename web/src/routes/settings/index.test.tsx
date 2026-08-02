import type { ReactElement } from "react";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { SettingsPage } from "./index";
import type { SessionInfo } from "@/api/session";
import type { CostBasisRules } from "@/api/tax-residencies";

// openapi-fetch captures globalThis.fetch at import time
// (`fetch: baseFetch = globalThis.fetch`), so the double has to be installed
// *before* the imports above run — hence vi.hoisted.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// jsdom doesn't implement scrollIntoView, but Radix's Select content calls
// it when positioning the open listbox — polyfill it as a no-op so the
// "open the select and click an item" flow doesn't throw.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

// Serves the given endpoints and 404s everything else, matching on the path's
// suffix. Path-aware rather than one blanket response because this screen
// reads two different endpoints — the session and the country list — and a
// blanket mock would hand the session object to whichever asked first.
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

// The bodies the PATCH was actually called with, in order — what the screen
// asked the server to change, as opposed to what it rendered afterwards.
// openapi-fetch hands globalThis.fetch a single Request object rather than
// (url, init), so both shapes are read: the Request is cloned before its body
// is consumed, since a body can only be read once.
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

// Britain diverges from what this application computes in BOTH ways at once,
// which is why it is the fixture here: it proves the screen shows every
// divergence rather than the first one.
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
    tax_residency: "RU",
    cost_basis_rules: RU_RULES,
    ...overrides,
  };
}

// SettingsPage reads the session straight from the query cache (useSession),
// so seeding ["session"] directly is the whole setup; the country list is
// served over the network like the real thing, because "the list comes from
// the server" is one of the things under test.
function wrap(ui: ReactElement, session: SessionInfo) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  return { qc, ...render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>) };
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
    });
  });

  it("shows the base currency form for an owner with Save disabled until something changes", () => {
    wrap(<SettingsPage />, makeSession({ base_currency: "RUB" }));

    expect(screen.getByText("Базовая валюта")).toBeInTheDocument();
    expect(currencySelect()).toBeInTheDocument();
    expect(saveButton()).toBeDisabled();
  });

  it("enables Save once a different currency is selected", () => {
    wrap(<SettingsPage />, makeSession({ base_currency: "RUB" }));

    fireEvent.click(currencySelect());
    fireEvent.click(screen.getByText("USD"));

    expect(saveButton()).toBeEnabled();
  });

  it("invalidates cached account balances after the base currency changes", async () => {
    // Account rows carry balance_in_base — money converted into the *old*
    // base currency. Leaving that cache alone would relabel those figures
    // with the new currency while they still hold the old one's arithmetic,
    // so the cached list has to be marked stale alongside the summary.
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
      "/api/v1/space": { body: makeSession({ base_currency: "USD" }) },
    });

    const { qc } = wrap(<SettingsPage />, makeSession({ base_currency: "RUB" }));
    qc.setQueryData(["accounts"], []);
    qc.setQueryData(["summary"], null);

    fireEvent.click(currencySelect());
    fireEvent.click(screen.getByText("USD"));
    fireEvent.click(saveButton());

    await waitFor(() => {
      expect(qc.getQueryState(["accounts"])?.isInvalidated).toBe(true);
    });
    expect(qc.getQueryState(["summary"])?.isInvalidated).toBe(true);
  });

  it("invalidates cached operations and positions after the base currency changes", async () => {
    // The account detail screen keeps the journal and the positions table
    // cached under ["operations", accountId, ...] / ["positions", accountId]
    // while it's mounted. Both carry figures converted into the *old* base
    // currency (in_base on operations, the base-converted columns on
    // positions). Leaving those two caches alone — while ["accounts"] and
    // ["summary"] do get invalidated — means a background refetch can land
    // between the currency switch and the account screen re-rendering, and
    // in that window the row shows an amount computed in the old base
    // currency under the new base currency's symbol.
    serve({
      "/api/v1/auth/me": { body: makeSession() },
      "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
      "/api/v1/space": { body: makeSession({ base_currency: "USD" }) },
    });

    const { qc } = wrap(<SettingsPage />, makeSession({ base_currency: "RUB" }));
    qc.setQueryData(["operations", "acc-1", 50, 0], []);
    qc.setQueryData(["positions", "acc-1"], []);

    fireEvent.click(currencySelect());
    fireEvent.click(screen.getByText("USD"));
    fireEvent.click(saveButton());

    await waitFor(() => {
      expect(qc.getQueryState(["operations", "acc-1", 50, 0])?.isInvalidated).toBe(true);
    });
    expect(qc.getQueryState(["positions", "acc-1"])?.isInvalidated).toBe(true);
  });

  it("shows an owner-only message and no form for a non-owner", () => {
    wrap(<SettingsPage />, makeSession({ role: "editor" }));

    expect(
      screen.getByText("Настройки доступны только владельцу пространства"),
    ).toBeInTheDocument();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Сохранить" })).not.toBeInTheDocument();
  });

  describe("tax residency", () => {
    it("offers exactly the countries the server sent, by name", async () => {
      // A list kept in the frontend would drift from the server's table the
      // first time a country is added there — offering one the server
      // rejects, or hiding one it accepts. So the options must come from the
      // response and nowhere else.
      wrap(<SettingsPage />, makeSession({ tax_residency: "RU" }));

      await waitFor(() => expect(countrySelect()).toBeEnabled());
      fireEvent.click(countrySelect());

      const options = screen.getAllByRole("option").map((o) => o.textContent);
      expect(options).toEqual(["Великобритания", "Германия", "Россия"]);
    });

    it("states what the selected country means for the figures before it is saved", async () => {
      wrap(<SettingsPage />, makeSession({ tax_residency: "RU" }));

      // The saved country is one the application does compute for, and it
      // says so rather than staying silent: the owner asked a question by
      // opening this screen.
      expect(
        await screen.findByText(/соответствует правилам этой страны/),
      ).toBeInTheDocument();

      await waitFor(() => expect(countrySelect()).toBeEnabled());
      fireEvent.click(countrySelect());
      fireEvent.click(screen.getByText("Великобритания"));

      // Both of Britain's divergences, from the list the server sent with
      // that country — no save round trip needed to learn them.
      const notice = screen.getByTestId("cost-basis-notice");
      expect(within(notice).getByText(/списываются не самые ранние покупки/)).toBeInTheDocument();
      expect(within(notice).getByText(/сразу по всем счетам владельца/)).toBeInTheDocument();
      expect(within(notice).queryByText(/соответствует правилам этой страны/)).toBeNull();
    });

    it("saves the country alone and refreshes what its statement travels with", async () => {
      serve({
        "/api/v1/auth/me": { body: makeSession() },
        "/api/v1/tax-residencies": { body: [RU_RULES, GB_RULES, DE_RULES] },
        "/api/v1/space": {
          body: makeSession({ tax_residency: "GB", cost_basis_rules: GB_RULES }),
        },
      });

      const { qc } = wrap(<SettingsPage />, makeSession({ tax_residency: "RU" }));
      qc.setQueryData(["positions", "acc-1"], []);

      await waitFor(() => expect(countrySelect()).toBeEnabled());
      fireEvent.click(countrySelect());
      fireEvent.click(screen.getByText("Великобритания"));
      fireEvent.click(saveButton());

      // cost_basis_rules travels with the positions payload, so a cache kept
      // from before the change would show the old country's statement over
      // the new country's figures.
      await waitFor(() => {
        expect(qc.getQueryState(["positions", "acc-1"])?.isInvalidated).toBe(true);
      });
      // Only what changed is sent: picking a country must not rewrite the
      // base currency as a side effect.
      expect(await patchBodies()).toEqual([{ tax_residency: "GB" }]);
    });
  });
});
