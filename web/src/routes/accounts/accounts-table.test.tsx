import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from "@tanstack/react-router";
import "@/i18n";
import { AccountsTable } from "./accounts-table";
import type { AccountWithBalance } from "@/api/accounts";
import { formatMinor } from "@/lib/money";

// AccountsTable renders row links via <Link to="/accounts/$accountId">,
// which needs a real router context to render at all (throws otherwise) —
// so wrap with the lightest possible router instead of a bare render.
function wrap(ui: ReactElement) {
  const rootRoute = createRootRoute();
  const testRoute = createRoute({ getParentRoute: () => rootRoute, path: "/", component: () => ui });
  const detailRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/accounts/$accountId",
    component: () => null,
  });
  const routeTree = rootRoute.addChildren([testRoute, detailRoute]);
  const router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: ["/"] }) });
  return render(<RouterProvider router={router} />);
}

// NBSP-insensitive compare: Intl.NumberFormat uses non-breaking spaces
// (matches the helper in money.test.ts / summary-cards.test.tsx).
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

function makeAccount(overrides: Partial<AccountWithBalance> = {}): AccountWithBalance {
  return {
    id: "acc-1",
    name: "Brokerage",
    type: "brokerage",
    currency: "USD",
    institution: "Broker Co",
    status: "active",
    created_at: "2026-01-01T00:00:00Z",
    balance: { as_of: "2026-07-20", amount_minor: 100_000 },
    ...overrides,
  };
}

describe("AccountsTable", () => {
  it("shows the native balance in native mode, ignoring any base figure", async () => {
    const account = makeAccount({
      balance_in_base: { amount_minor: 900_000, currency: "RUB", rate_on: "2026-07-20" },
    });
    wrap(<AccountsTable accounts={[account]} mode="native" baseCurrency="RUB" />);

    const amount = await screen.findByTestId("account-balance-acc-1");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(100_000, "USD")));
  });

  it("shows the converted balance in base mode when balance_in_base is present", async () => {
    const account = makeAccount({
      balance_in_base: { amount_minor: 900_000, currency: "RUB", rate_on: "2026-07-20" },
    });
    wrap(<AccountsTable accounts={[account]} mode="base" baseCurrency="RUB" />);

    const amount = await screen.findByTestId("account-balance-acc-1");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(900_000, "RUB")));
    expect(screen.queryByTestId("account-balance-acc-1-not-converted")).not.toBeInTheDocument();
  });

  it("prints the converted balance in the currency the balance itself carries, not the session's", async () => {
    // #106, in the shape the owner meets it: settings changes the base
    // currency, the session's new answer lands in the cache at once
    // (useUpdateSpace writes it directly), and this list still holds figures
    // the server converted into the OLD base currency until its refetch comes
    // back. The rubles must keep printing as rubles for that window — a euro
    // sign over them is not a mislabelling, it is a number wrong by the whole
    // exchange rate with nothing on screen admitting it.
    const account = makeAccount({
      balance_in_base: { amount_minor: 900_000, currency: "RUB", rate_on: "2026-07-20" },
    });
    wrap(<AccountsTable accounts={[account]} mode="base" baseCurrency="EUR" />);

    const amount = await screen.findByTestId("account-balance-acc-1");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(900_000, "RUB")));
    expect(amount.textContent).not.toContain("€");
  });

  it("discloses the fx rate date of a converted balance in the cell's tooltip, not as text", async () => {
    const account = makeAccount({
      balance_in_base: { amount_minor: 900_000, currency: "RUB", rate_on: "2026-07-19" },
    });
    wrap(<AccountsTable accounts={[account]} mode="base" baseCurrency="RUB" />);

    const amount = await screen.findByTestId("account-balance-acc-1");
    expect(amount).toHaveAttribute("title", "Пересчитано по текущему курсу (на 19.07.2026)");
    // The balance row already shows the balance's own as_of date as text;
    // the rate date must not join it there.
    expect(amount.textContent).not.toMatch(/19\.07\.2026/);
  });

  it("shows no rate-date tooltip in native mode — nothing was converted", async () => {
    const account = makeAccount({
      balance_in_base: { amount_minor: 900_000, currency: "RUB", rate_on: "2026-07-19" },
    });
    wrap(<AccountsTable accounts={[account]} mode="native" baseCurrency="RUB" />);

    expect(await screen.findByTestId("account-balance-acc-1")).not.toHaveAttribute("title");
  });

  it("shows the native balance plus a not-converted indicator in base mode when no rate is available", async () => {
    const account = makeAccount({ currency: "USD", balance_in_base: null });
    wrap(<AccountsTable accounts={[account]} mode="base" baseCurrency="RUB" />);

    const amount = await screen.findByTestId("account-balance-acc-1");
    // Honesty rule: the real native amount, never a dash or a fake zero.
    expect(amount.textContent).toContain(formatMinor(100_000, "USD"));
    const indicator = screen.getByTestId("account-balance-acc-1-not-converted");
    expect(indicator).toHaveAttribute("title", "Нет курса — показано в валюте счёта");
  });

  it("shows the plain native balance in base mode with no indicator when the account's currency already is the base currency", async () => {
    const account = makeAccount({ currency: "RUB", balance_in_base: null });
    wrap(<AccountsTable accounts={[account]} mode="base" baseCurrency="RUB" />);

    const amount = await screen.findByTestId("account-balance-acc-1");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(100_000, "RUB")));
    expect(screen.queryByTestId("account-balance-acc-1-not-converted")).not.toBeInTheDocument();
  });

  it("still shows the honest dash for accounts with no balance at all, regardless of mode", async () => {
    const account = makeAccount({ balance: undefined });
    wrap(<AccountsTable accounts={[account]} mode="base" baseCurrency="RUB" />);

    await screen.findByText("Brokerage");
    expect(screen.getByText("—")).toBeInTheDocument();
    expect(screen.queryByTestId("account-balance-acc-1")).not.toBeInTheDocument();
  });
});
