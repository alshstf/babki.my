import type { ReactElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
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

function wrap(ui: ReactElement) {
  const rootRoute = createRootRoute();
  const testRoute = createRoute({ getParentRoute: () => rootRoute, path: "/", component: () => ui });
  const detailRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/accounts/$accountId",
    component: () => null,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([testRoute, detailRoute]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  return render(<RouterProvider router={router} />);
}

const norm = (s: string) => s.replace(/[  ]/g, " ");

type Reconciliation = NonNullable<NonNullable<AccountWithBalance["journal"]>["reconciliation"]>;

function brokerage(
  reconciliation: Reconciliation | null,
  overrides: Partial<AccountWithBalance> = {},
): AccountWithBalance {
  return {
    id: "alfa",
    name: "Альфа",
    type: "brokerage",
    currency: "RUB",
    institution: "",
    status: "active",
    created_at: "2026-01-01T00:00:00Z",
    valued_by_balance: false,
    counted_by: "journal",
    balance: { as_of: "2026-10-02", amount_minor: 54_000_000 },
    journal: {
      amount_minor: 19_500_000,
      full_amount_minor: 19_500_000,
      currency: "RUB",
      unpriced_positions: 0,
      not_traded_positions: 0,
      missing_rates: [],
      negative_cash: [],
      reconciliation,
    },
    ...overrides,
  };
}

const differs: Reconciliation = {
  status: "differs",
  balance_as_of: "2026-10-02",
  compared_on: "2026-10-02",
  balance_in_base_minor: 54_000_000,
  difference_minor: -34_500_000,
};

describe("a brokerage account counted by its journal", () => {
  it("shows the journal's figure, says it differs from the balance and offers the balance", async () => {
    const onValueBy = vi.fn();
    const account = brokerage(differs);
    wrap(<AccountsTable accounts={[account]} mode="native" baseCurrency="RUB" onValueBy={onValueBy} />);

    const value = await screen.findByTestId("account-journal-value-alfa");
    expect(norm(value.textContent ?? "")).toBe(norm(formatMinor(19_500_000, "RUB")));
    expect(screen.queryByTestId("account-balance-alfa")).not.toBeInTheDocument();
    expect(screen.getByText("по журналу операций")).toBeInTheDocument();
    expect(norm(screen.getByTestId("account-reconciliation-alfa").textContent ?? "")).toBe(
      norm(
        `не сходится с балансом ${formatMinor(54_000_000, "RUB")} на 02.10.2026: разница ${formatMinor(-34_500_000, "RUB")} — похоже, в журнале не хватает операций`,
      ),
    );

    fireEvent.click(screen.getByRole("button", { name: "считать по балансу" }));
    expect(onValueBy).toHaveBeenCalledWith(account, true);
  });

  it("offers nothing when the journal agrees, and nothing to a viewer", async () => {
    const agrees = brokerage({ ...differs, status: "agrees", difference_minor: -21_000 });
    wrap(<AccountsTable accounts={[agrees]} mode="native" baseCurrency="RUB" onValueBy={vi.fn()} />);
    expect(norm((await screen.findByTestId("account-reconciliation-alfa")).textContent ?? "")).toBe(
      norm(
        `сходится с балансом ${formatMinor(54_000_000, "RUB")} на 02.10.2026: разница ${formatMinor(-21_000, "RUB")}`,
      ),
    );
    expect(screen.queryByRole("button", { name: "считать по балансу" })).not.toBeInTheDocument();
  });

  it("hides the switch from a viewer even where the journal differs", async () => {
    wrap(<AccountsTable accounts={[brokerage(differs)]} mode="native" baseCurrency="RUB" />);
    await screen.findByTestId("account-reconciliation-alfa");
    expect(screen.queryByRole("button", { name: "считать по балансу" })).not.toBeInTheDocument();
  });

  it("names what it could not count", async () => {
    const account = brokerage(null);
    account.journal = {
      ...account.journal!,
      unpriced_positions: 2,
      negative_cash: ["USD"],
      missing_rates: ["EUR"],
    };
    wrap(<AccountsTable accounts={[account]} mode="native" baseCurrency="RUB" />);
    expect(await screen.findByText("бумаг без цены посчитано нулём: 2")).toBeInTheDocument();
    expect(
      screen.getByText("деньги ушли в минус: USD — похоже, не записано пополнение"),
    ).toBeInTheDocument();
    expect(screen.getByText("не вошло, нет курса: EUR")).toBeInTheDocument();
    expect(screen.queryByTestId("account-reconciliation-alfa")).not.toBeInTheDocument();
  });

  it("gives no verdict against an old balance", async () => {
    wrap(
      <AccountsTable
        accounts={[brokerage({ ...differs, status: "stale", balance_as_of: "2026-03-12", compared_on: "2026-03-12" })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );
    expect(norm((await screen.findByTestId("account-reconciliation-alfa")).textContent ?? "")).toBe(
      norm(
        `последний баланс ${formatMinor(54_000_000, "RUB")} — от 12.03.2026; сверить не по чему`,
      ),
    );
  });
});

describe("a brokerage account pinned to its balance", () => {
  it("shows the balance, the journal beside it, and the way back", async () => {
    const onValueBy = vi.fn();
    const account = brokerage(differs, { valued_by_balance: true, counted_by: "balance" });
    wrap(<AccountsTable accounts={[account]} mode="native" baseCurrency="RUB" onValueBy={onValueBy} />);

    const balance = await screen.findByTestId("account-balance-alfa");
    expect(norm(balance.textContent ?? "")).toBe(norm(formatMinor(54_000_000, "RUB")));
    expect(screen.queryByTestId("account-journal-value-alfa")).not.toBeInTheDocument();
    expect(norm(screen.getByTestId("account-journal-pinned-alfa").textContent ?? "")).toBe(
      norm(`считается по балансу — так выбрано; по журналу ${formatMinor(19_500_000, "RUB")}`),
    );

    fireEvent.click(screen.getByRole("button", { name: "считать по журналу" }));
    expect(onValueBy).toHaveBeenCalledWith(account, false);
  });

  it("names the day an old balance was compared on", async () => {
    wrap(
      <AccountsTable
        accounts={[
          brokerage({ ...differs, balance_as_of: "2026-03-12", compared_on: "2026-03-12" }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );
    expect((await screen.findByTestId("account-reconciliation-alfa")).textContent).toContain("на 12.03.2026");
  });
});
