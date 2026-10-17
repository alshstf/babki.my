import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from "@tanstack/react-router";
import "@/i18n";
import { IncomePanel } from "./income-panel";
import type { AccountWithBalance } from "@/api/accounts";
import type { AccountIncome } from "@/api/account-income";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const account: AccountWithBalance = {
  id: "dep-1", name: "Вклад", type: "deposit", currency: "RUB", institution: "", status: "active",
  created_at: "2026-01-01T00:00:00Z", valued_by_balance: false, trades_abroad: false, kept_by_operations: false, foreign_broker: false, tax_owner_user_id: null, counted_by: "balance",
};

const none: AccountIncome = {
  currency: "RUB", tax_currency: "RUB", year: 2026, rated: false, ends_on: null, payout: null, term_minor: null, accrued_minor: null, at_end_minor: null,
  month_minor: null, month_on: null, twelve_minor: null, year_end_minor: null, year_minor: null, estimate: null, tax: null,
};

const estimate = { paid_minor: 0, from: null, ahead_minor: 0, missing: null, missing_on: null, no_rate_ahead: false, pays_on: null };

function show(income: AccountIncome) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(new Response(JSON.stringify(income), { status: 200, headers: { "Content-Type": "application/json" } })),
  );
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const root = createRootRoute({ component: () => <IncomePanel account={account} /> });
  const taxes = createRoute({ getParentRoute: () => root, path: "/taxes", component: () => null });
  const router = createRouter({ routeTree: root.addChildren([taxes]), history: createMemoryHistory({ initialEntries: ["/"] }) });
  render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("IncomePanel", () => {
  // Р-56 Б: Мария's «Годовой», 1 500 000 ₽ for a year at 18 %, paying at the
  // end; her tax on interest 23 400 ₽, this deposit's share by its interest.
  it("lists a deposit's income and its share of the person's tax, said to be a split", async () => {
    show({
      ...none, rated: true, ends_on: "2026-12-20", payout: "end", term_minor: 270_000_00, accrued_minor: 222_657_53, at_end_minor: 1_770_000_00,
      year_end_minor: 270_000_00, year_minor: 270_000_00, estimate: { ...estimate, ahead_minor: 270_000_00 },
      tax: { person: "Мария", interest_minor: 340_000_00, free_minor: 160_000_00, tax_minor: 23_400_00, share_minor: 18_582_35, unestimated: ["Сейф"] },
    });
    const panel = await screen.findByTestId("account-income");
    const rows = [...panel.querySelectorAll("dl > div")].map((d) => norm(d.textContent));
    expect(rows).toEqual([
      "Проценты за весь срок, до 20.12.2026270 000,00 ₽",
      "Начислено к сегодня222 657,53 ₽",
      "На счёте 20.12.20261 770 000,00 ₽",
      "До 31 декабря придёт270 000,00 ₽",
      "Проценты за 2026 год, для налога270 000,00 ₽",
    ]);
    expect(norm(screen.getByTestId("account-income-notes").textContent)).toBe(
      "Дни впереди — по сегодняшней ставке, ставка от ключевой — по сегодняшней ключевой. Проценты — в конце срока. После срока не считаем: ставка продления заранее не известна.",
    );
    expect(norm(screen.getByTestId("account-income-tax").textContent)).toBe(
      "Налог на проценты считается по всем вкладам и счетам вместе (Мария): за 2026 год ≈ 23 400,00 ₽ с ≈ 340 000,00 ₽ процентов, без налога 160 000,00 ₽." +
        "Доля этого счёта — ≈ 18 582,35 ₽: налог делим между счетами пропорционально их процентам. В уведомлении ФНС будет только общая сумма." +
        "Не вошли — проценты не оценить: Сейф." +
        "Подробнее — «Налоги»",
    );
  });

  it("says a savings account's month and year, no tax under the free part, and the first balance mark", async () => {
    show({
      ...none, rated: true, month_minor: 32_749_77, month_on: "2026-10-31", twelve_minor: 407_531_70, year_end_minor: 98_175_97, year_minor: 311_527_52,
      estimate: { ...estimate, paid_minor: 213_351_55, ahead_minor: 98_175_97, from: "2026-03-05" },
      tax: { person: "Мария", interest_minor: 120_000_00, free_minor: 160_000_00, tax_minor: 0, share_minor: 0, unestimated: [] },
    });
    const panel = await screen.findByTestId("account-income");
    expect(norm(panel.textContent)).toMatch(/В этом месяце придёт, 31\.10\.202632 749,77 ₽/);
    expect(norm(panel.textContent)).toMatch(/За 12 месяцев407 531,70 ₽/);
    expect(norm(screen.getByTestId("account-income-notes").textContent)).toMatch(/Считается с 05\.03\.2026 — до первой отметки остатка денег на счёте не видно\./);
    expect(norm(screen.getByTestId("account-income-tax").textContent)).toMatch(
      /^Налога на проценты за 2026 год не будет \(Мария\): ≈ 120 000,00 ₽ процентов по всем вкладам и счетам — в пределах 160 000,00 ₽ без налога\./,
    );
    expect(screen.queryByText(/Доля этого счёта/)).toBeNull();
  });

  it("says when there is no rate to reckon by", async () => {
    show(none);
    expect(norm((await screen.findByTestId("account-income")).textContent)).toBe(
      "Ставки нет — доход по ней не посчитать. Ставку можно вписать или взять из программы банка выше.",
    );
  });

  it("names why a deposit's interest of the year is not known", async () => {
    show({ ...none, estimate: { ...estimate, missing: "how_paid" } });
    expect(norm((await screen.findByTestId("account-income-notes")).textContent)).toBe(
      "Строки дохода не посчитать: нет ставки на все нужные дни или не известно, как банк платит проценты. Не известно, как банк платит проценты: у вклада нет программы банка или не указан способ выплаты.",
    );
  });
});
