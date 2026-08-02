import type { ReactElement } from "react";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { SettingsPage } from "./index";
import type { SessionInfo } from "@/api/session";

// openapi-fetch captures globalThis.fetch at import time
// (`fetch: baseFetch = globalThis.fetch`), so the double has to be installed
// *before* the imports above run — hence vi.hoisted. Only the Save case
// touches the network; the rest never call it.
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

// SettingsPage reads the session straight from the query cache (useSession),
// so seeding ["session"] directly is the whole setup — the cases that don't
// click Save need no network mocking at all.
function wrap(ui: ReactElement, session: SessionInfo) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  return { qc, ...render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>) };
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

describe("SettingsPage", () => {
  beforeEach(() => {
    fetchMock.mockReset();
  });

  it("shows the base currency form for an owner with Save disabled until the currency changes", () => {
    wrap(<SettingsPage />, makeSession({ base_currency: "RUB" }));

    expect(screen.getByText("Базовая валюта")).toBeInTheDocument();
    expect(screen.getByRole("combobox")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Сохранить" })).toBeDisabled();
  });

  it("enables Save once a different currency is selected", () => {
    wrap(<SettingsPage />, makeSession({ base_currency: "RUB" }));

    fireEvent.click(screen.getByRole("combobox"));
    fireEvent.click(screen.getByText("USD"));

    expect(screen.getByRole("button", { name: "Сохранить" })).toBeEnabled();
  });

  it("invalidates cached account balances after the base currency changes", async () => {
    // Account rows carry balance_in_base — money converted into the *old*
    // base currency. Leaving that cache alone would relabel those figures
    // with the new currency while they still hold the old one's arithmetic,
    // so the cached list has to be marked stale alongside the summary.
    // A fresh Response per call: a body can only be read once, and
    // useSession refetches /auth/me on mount alongside the PATCH.
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify(makeSession({ base_currency: "USD" })), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );

    const { qc } = wrap(<SettingsPage />, makeSession({ base_currency: "RUB" }));
    qc.setQueryData(["accounts"], []);
    qc.setQueryData(["summary"], null);

    fireEvent.click(screen.getByRole("combobox"));
    fireEvent.click(screen.getByText("USD"));
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

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
    // currency under the new base currency's symbol. Same fresh-Response
    // rationale as the sibling test above.
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify(makeSession({ base_currency: "USD" })), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );

    const { qc } = wrap(<SettingsPage />, makeSession({ base_currency: "RUB" }));
    qc.setQueryData(["operations", "acc-1", 50, 0], []);
    qc.setQueryData(["positions", "acc-1"], []);

    fireEvent.click(screen.getByRole("combobox"));
    fireEvent.click(screen.getByText("USD"));
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

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
});
