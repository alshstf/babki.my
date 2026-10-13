import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { TaxesPage } from "./index";
import type { TaxYear } from "@/api/tax-year";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

// Alex with two Russian brokers past the threshold and a broker abroad;
// Maria whose broker took more than her year's tax.
const year: TaxYear = {
  year: 2026, supported: true, currency: "RUB", declare_by: "2027-04-30", pay_by: "2027-07-15", notice_by: "2027-12-01", key_rate: "21.00", by_balance: 1,
  to_pay_minor: 32_000_00, refund_minor: 8_000_00,
  people: [
    {
      user_id: "u-alex", name: "Алекс", base_minor: 4_000_000_00, tax_minor: 552_000_00, by_agents_minor: 520_000_00, withheld_minor: 6_500_00,
      credit_minor: 0, to_pay_minor: 32_000_00, refund_minor: 0, unvalued: 2, interest_minor: 400_000_00, interest_free_minor: 210_000_00, by_notice_minor: 28_500_00,
      wages_net_minor: 0, wages_minor: 0, wages_tax_minor: 0, wages_withheld_minor: 0, wages_to_pay_minor: 0, employers: [], wage_payments: [],
      property_sold_minor: 0, property_gain_minor: 0, property_base_minor: 0, property_free: 0,
      accounts: [
        { account_id: "a-1", name: "Т-Банк", bank: false, foreign_broker: false, sales_minor: 2_400_000_00, sales_long_minor: 400_000_00, coupons_minor: 0, dividends_minor: 0, interest_minor: 0, withheld_minor: 6_500_00, securities_minor: 2_000_000_00, agent_tax_minor: 260_000_00, unvalued: 2 },
        { account_id: "a-2", name: "Freedom KZ", bank: false, foreign_broker: true, sales_minor: 0, sales_long_minor: 0, coupons_minor: 0, dividends_minor: 100_000_00, interest_minor: 0, withheld_minor: 10_000_00, securities_minor: 0, agent_tax_minor: 0, unvalued: 0 },
        { account_id: "a-4", name: "Накопительный", bank: true, foreign_broker: false, sales_minor: 0, sales_long_minor: 0, coupons_minor: 0, dividends_minor: 0, interest_minor: 400_000_00, withheld_minor: 0, securities_minor: 0, agent_tax_minor: 0, unvalued: 0 },
      ],
    },
    {
      user_id: "u-maria", name: "Мария", base_minor: 100_000_00, tax_minor: 13_000_00, by_agents_minor: 21_000_00, withheld_minor: 0,
      credit_minor: 0, to_pay_minor: 0, refund_minor: 8_000_00, unvalued: 0, accounts: [], interest_minor: 0, interest_free_minor: 210_000_00, by_notice_minor: 0,
      wages_net_minor: 87_000_00, wages_minor: 100_000_00, wages_tax_minor: 13_000_00, wages_withheld_minor: 13_000_00, wages_to_pay_minor: 0,
      employers: [{ name: "ООО «Ромашка»", net_minor: 87_000_00, gross_minor: 100_000_00, withheld_minor: 13_000_00 }],
      wage_payments: [{ operation_id: "op-1", on: "2026-03-05", employer: "ООО «Ромашка»", net_minor: 87_000_00, gross_minor: 100_000_00, withheld_minor: 13_000_00, entered: false }],
      property_sold_minor: 525_000_00, property_gain_minor: 115_000_00, property_base_minor: 115_000_00, property_free: 1,
    },
  ],
};

const shared = {
  id: "a-3", name: "Общий брокер", type: "brokerage", currency: "RUB", institution: "", status: "active", created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false, trades_abroad: false, kept_by_operations: false, foreign_broker: false, tax_owner_user_id: null, counted_by: "journal", owner_user_id: null,
};

function answer(y: TaxYear) {
  fetchMock.mockImplementation((input: Request) => {
    const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
    const path = new URL(input.url).pathname;
    if (path.endsWith("/tax-year")) return json(y);
    if (path.endsWith("/accounts")) return json([shared]);
    if (path.endsWith("/members")) return json([{ id: "u-alex", username: "a", display_name: "Алекс", role: "owner" }, { id: "u-maria", username: "m", display_name: "Мария", role: "editor" }]);
    if (path.endsWith("/auth/me")) return json({ user: { id: "u-alex", username: "a", display_name: "Алекс" }, role: "owner", space: { id: "s", name: "S", base_currency: "RUB" } });
    if (input.method === "PATCH") return json(shared);
    if (path.includes("/tax-year/wages/")) return Promise.resolve(new Response(null, { status: 204 }));
    return json(null);
  });
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TaxesPage />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("TaxesPage", () => {
  it("says per person what is left to pay, or to get back, and why", async () => {
    answer(year);
    show();
    const people = await screen.findAllByTestId("tax-person");
    expect(norm(within(people[0]).getByTestId("tax-verdict").textContent)).toBe("Доплатить самим: 32 000,00 ₽");
    expect(norm(people[0].textContent)).toMatch(/Декларация 3-НДФЛ — до 30\.04\.2027, оплата — до 15\.07\.2027\./);
    expect(norm(within(people[0]).getByTestId("tax-summary").textContent)).toBe("Налог за год ≈ 552 000,00 ₽ с дохода 4 000 000,00 ₽ (13 % до 2,4 млн, 15 % сверх).");
    expect(norm(within(people[0]).getByTestId("tax-agents").textContent)).toBe("Удержат брокеры 520 000,00 ₽ · уже удержано 6 500,00 ₽");
    expect(norm(people[0].textContent)).toMatch(/Не учтено продаж и выплат без даты покупки или курса: 2/);
    const rows = within(people[0]).getAllByTestId("tax-account");
    expect(norm(rows[0].textContent)).toMatch(/Т-Банк2 400 000,00 ₽из них 400 000,00 ₽ — бумаги дольше трёх лет, без налога/);
    expect(norm(rows[1].textContent)).toMatch(/Freedom KZиностранный брокер.*сами, по декларации/);
    expect(norm(within(people[1]).getByTestId("tax-verdict").textContent)).toBe("Можно вернуть по декларации: 8 000,00 ₽");
    // Interest: free up to the key rate's 210 000, the rest by notice; the
    // bank's account is not a row of the brokers' table.
    expect(norm(within(people[0]).getByTestId("tax-interest").textContent)).toBe(
      "Проценты по вкладам и счетам: 400 000,00 ₽, из них без налога 210 000,00 ₽ (1 млн × наибольшая ключевая ставка года, 21 %). Налог с остального ≈ 28 500,00 ₽ — ФНС пришлёт уведомление, оплата до 01.12.2027; он уже в налоге за год, в «доплатить самим» не входит.",
    );
    expect(rows).toHaveLength(2);
    expect(within(people[1]).queryByTestId("tax-interest")).toBeNull();
    expect(norm(screen.getByTestId("tax-by-balance").textContent)).toMatch(/ведутся остатком: 1 —/);
    expect(screen.getByText(/НКД, уплаченный при покупке облигаций, здесь уменьшает налог при продаже или погашении/)).toBeTruthy();
    // Maria's wages, reckoned from what reached her account.
    expect(norm(within(people[1]).getByTestId("tax-wages").textContent)).toBe(
      "Зарплата: на руки 87 000,00 ₽, до налога ≈ 100 000,00 ₽ — расчёт программы по сумме на руки. Налог по шкале ≈ 13 000,00 ₽, работодатели удержали ≈ 13 000,00 ₽.",
    );
    expect(within(people[0]).queryByTestId("tax-wages")).toBeNull();
    expect(norm(within(people[1]).getByTestId("tax-property").textContent)).toBe(
      "Продажа слитков и монет: выручка 525 000,00 ₽, прибыль 115 000,00 ₽; к налогу 115 000,00 ₽ — меньшее из прибыли и выручки без вычета 250 000 ₽. Банк налог не удерживает — по декларации. Проданных после трёх лет владения — без налога: 1.",
    );
  });

  it("writes a wage payment's payslip, accepts the reckoning, or drops it", async () => {
    answer(year);
    show();
    const people = await screen.findAllByTestId("tax-person");
    fireEvent.click(within(people[1]).getByRole("button", { name: "Выплаты и суммы из расчётных листков (1)" }));
    const row = within(people[1]).getByTestId("tax-wage-payment");
    expect(norm(row.textContent)).toMatch(/05\.03\.2026 · ООО «Ромашка»: на руки 87 000,00 ₽, до налога 100 000,00 ₽, удержано 13 000,00 ₽· расчёт программы/);
    fireEvent.click(within(row).getByRole("button", { name: "Вписать из листка" }));
    fireEvent.change(within(row).getByLabelText("До налога, ₽"), { target: { value: "115 000" } });
    fireEvent.click(within(row).getByRole("button", { name: "Сохранить" }));
    const sent = async (method: string) => {
      await waitFor(() => expect(fetchMock.mock.calls.map(([r]) => r as Request).some((r) => r.method === method)).toBe(true));
      return fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === method)!;
    };
    const put = await sent("PUT");
    expect(put.url).toMatch(/\/tax-year\/wages\/op-1$/);
    expect(await put.json()).toEqual({ gross_minor: 115_000_00, withheld_minor: 13_000_00 });
    fetchMock.mockClear();
    fireEvent.click(within(row).getByRole("button", { name: "Принять расчёт" }));
    expect(await (await sent("PUT")).json()).toEqual({ gross_minor: 100_000_00, withheld_minor: 13_000_00 });
  });

  it("says a payslip is in, and drops it", async () => {
    const p = year.people[1];
    answer({ ...year, people: [year.people[0], { ...p, wage_payments: [{ ...p.wage_payments[0], gross_minor: 115_000_00, entered: true }] }] });
    show();
    const people = await screen.findAllByTestId("tax-person");
    expect(norm(within(people[1]).getByTestId("tax-wages").textContent)).toMatch(/— по расчётным листкам\./);
    fireEvent.click(within(people[1]).getByRole("button", { name: "Выплаты и суммы из расчётных листков (1)" }));
    const row = within(people[1]).getByTestId("tax-wage-payment");
    expect(norm(row.textContent)).toMatch(/до налога 115 000,00 ₽.*· из листка/);
    fireEvent.click(within(row).getByRole("button", { name: "Вернуть расчёт программы" }));
    await waitFor(() => {
      const del = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "DELETE");
      expect(del?.url).toMatch(/\/tax-year\/wages\/op-1$/);
    });
  });

  it("asks whose a shared broker's account is for tax", async () => {
    answer(year);
    show();
    const box = await screen.findByTestId("tax-shared");
    expect(norm(box.textContent)).toMatch(/на владельца пространства \(Алекс\)/);
    fireEvent.click(within(box).getByRole("combobox", { name: "Чей доход для налога: Общий брокер" }));
    fireEvent.click(await screen.findByRole("option", { name: "Мария" }));
    await waitFor(() => {
      const patch = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PATCH");
      expect(patch?.url).toMatch(/\/accounts\/a-3$/);
    });
    const patch = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PATCH")!;
    expect(await patch.json()).toEqual({ tax_owner_user_id: "u-maria" });
  });

  it("says when the country has no estimate", async () => {
    answer({ ...year, supported: false, currency: "", people: [], to_pay_minor: 0, refund_minor: 0, key_rate: null, by_balance: 0 });
    show();
    expect(await screen.findByText(/только для налоговых резидентов России/)).toBeTruthy();
    expect(screen.queryByTestId("tax-person")).toBeNull();
  });
});
