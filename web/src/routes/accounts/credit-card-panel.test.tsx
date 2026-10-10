import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import type { ReactElement } from "react";
import "@/i18n";
import { CreditCardPanel } from "./credit-card-panel";
import { CardReminders } from "./card-reminders";
import type { AccountWithBalance } from "@/api/accounts";
import type { CreditCard, CreditCardStatus } from "@/api/credit-cards";
import { localToday } from "@/lib/dates";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const norm = (s: string) => s.replace(/[  ]/g, " ");
const inDays = (n: number) => {
  const d = new Date(Date.parse(localToday()) + n * 86_400_000);
  return d.toISOString().slice(0, 10);
};
const ru = (iso: string) => iso.split("-").reverse().join(".");

const account: AccountWithBalance = {
  id: "card-1", name: "Кредитка Альфа", type: "credit_card", currency: "RUB", institution: "", status: "active",
  created_at: "2026-01-01T00:00:00Z", valued_by_balance: false, trades_abroad: false, kept_by_operations: true,
  counted_by: "journal", balance: { as_of: "2026-07-20", amount_minor: 0 },
};

const noFees = {
  monthly_minor: 0, yearly_minor: 0, cash_free_minor: 0, cash_percent: "0", cash_fixed_minor: 0,
  transfer_free_minor: 0, transfer_percent: "0", transfer_fixed_minor: 0, intro_days: 0, intro_free_minor: 0,
  penalty_daily_percent: "0", penalty_yearly_percent: "0", penalty_from_day: 0,
};

const noInstallment = { months: 0, monthly_fee_percent: "0", fee_minor: 0 };

const noGraces = { grace_moves: false, grace_periods: 0, grace_categories: [], grace_to_month_end: false };

// The catalog as the server gives it: Газпромбанк's card by the contract's
// day.
const catalogFixture = [{
  id: "gpb-180-premium", bank: "Газпромбанк", card: "«180 дней Премиум»",
  versions: [{
    contracts_from: "2025-10-01", contracts_to: null, revision: "2026-09-10", checked_on: "2026-10-10",
    sources: ["https://example.test/tariff.pdf"], notes: "Обслуживание 590 ₽ в месяц не берётся при подписке.",
    terms: {
      statement_day: 1, grace_kind: "windows", window_months: 2, grace_months: 6, grace_all_lost: true, pay_by_period_end: true,
      charges_in_full: true, min_percent: "3", min_floor_minor: 50000, annual_rate: "59.99",
      fees: { cash_free_minor: 10000000, cash_percent: "5.9", cash_fixed_minor: 59000, transfer_percent: "4.9", transfer_fixed_minor: 39000, penalty_daily_percent: "0.1" },
    },
  }, {
    contracts_from: "2025-04-01", contracts_to: "2025-09-30", revision: "2026-09-10", checked_on: "2026-10-10",
    sources: ["https://example.test/old.pdf"], notes: "", terms: { grace_kind: "windows", window_months: 2, grace_months: 6 },
  }],
}];

const noCashback = { base_percent: "0", categories: [], monthly_cap_minor: 0, points: false, credit_days: 0 };

const status = (over: Partial<CreditCardStatus> = {}): CreditCardStatus => ({
  debt_minor: 57_300_00, available_minor: 92_700_00, last_statement: inDays(-9), next_statement: inDays(22),
  minimum_minor: 1_569_00, minimum_on: inDays(11), minimum_missed: false, minimum_estimate: false,
  grace: [{ on: inDays(5), amount_minor: 52_300_00 }, { on: inDays(36), amount_minor: 5_000_00 }],
  lost: [], non_grace_minor: 0, non_grace_interest_minor: 0,
  grace_off_since: null, grace_off_by_minimum: false, to_restore_minor: 0, cash_this_period_minor: 0, penalty_minor: 0,
  transfers_this_period_minor: 0, intro_left_minor: 0, intro_until: null, yearly_fee_on: null,
  cashback_expected_minor: 0, cashback_on: null, minimum_overdue_minor: 0,
  installments: [], installments_due_minor: 0, bank: null, ...over,
});

const card = (over: Partial<CreditCardStatus> = {}, byJournal = true, benefit: CreditCard["benefit"] = null): CreditCard => ({
  terms: { limit_minor: 150_000_00, statement_day: 1, payment_days: 20, grace_kind: "statement", grace_days: 0, grace_run_from: "purchase", pay_day: 0, min_round_up_minor: 0,
    missed_minimum_period: false,
    min_percent: "3", min_floor_minor: 300_00, annual_rate: "39.9", own_rate: benefit?.own_rate_known ? "15" : null,
    window_months: 0, grace_months: 0, opened_on: null, grace_all_lost: false, pay_by_period_end: false, charges_in_full: false,
    transfer_categories: [], ...noGraces, fees: noFees, cashback: noCashback, installment: noInstallment, catalog: null },
  status: status(over), by_journal: byJournal, benefit, catalog_update: null,
});

function answer(routes: Record<string, unknown>) {
  fetchMock.mockImplementation((input: Request) => {
    const path = new URL(input.url).pathname;
    const hit = Object.entries(routes).find(([p]) => path.endsWith(p));
    const body = hit ? hit[1] : { error: "not found" };
    return Promise.resolve(new Response(JSON.stringify(input.method === "PUT" ? card() : body), {
      status: hit || input.method === "PUT" ? 200 : 404, headers: { "Content-Type": "application/json" },
    }));
  });
}

function show(ui: ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const root = createRootRoute();
  const page = createRoute({ getParentRoute: () => root, path: "/", component: () => ui });
  const detail = createRoute({ getParentRoute: () => root, path: "/accounts/$accountId", component: () => null });
  const router = createRouter({ routeTree: root.addChildren([page, detail]), history: createMemoryHistory({ initialEntries: ["/"] }) });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("CreditCardPanel", () => {
  it("states the terms from a preset and the numbers typed", async () => {
    answer({});
    show(<CreditCardPanel account={account} canEdit />);
    fireEvent.click(await screen.findByRole("button", { name: "Указать условия карты" }));
    fireEvent.click(await screen.findByRole("button", { name: "120 дней (как СберКарта)" }));
    fireEvent.change(screen.getByLabelText(/Кредитный лимит/), { target: { value: "150000" } });
    fireEvent.change(screen.getByLabelText(/Ставка без льготы/), { target: { value: "39,9" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(await put.json()).toEqual({
      limit_minor: 150_000_00, statement_day: 1, payment_days: 20, grace_kind: "long", grace_days: 120, grace_run_from: "purchase", pay_day: 0, min_round_up_minor: 0,
      missed_minimum_period: false,
      min_percent: "3", min_floor_minor: 300_00, annual_rate: "39.9", own_rate: null,
      window_months: 0, grace_months: 0, opened_on: null, grace_all_lost: false, pay_by_period_end: false, charges_in_full: true,
      transfer_categories: [], ...noGraces, fees: noFees, cashback: noCashback, installment: noInstallment, catalog: null,
    });
  });

  it("takes a card's terms from the catalog by the contract's day", async () => {
    answer({ "/credit-cards/catalog": catalogFixture });
    Element.prototype.scrollIntoView ??= () => {};
    show(<CreditCardPanel account={account} canEdit />);
    fireEvent.click(await screen.findByRole("button", { name: "Указать условия карты" }));
    const box = await screen.findByTestId("card-catalog");
    fireEvent.keyDown(within(box).getByRole("combobox", { name: "Карта" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "Газпромбанк «180 дней Премиум»" }));
    fireEvent.change(within(box).getByLabelText("Дата договора"), { target: { value: "2026-07-10" } });
    expect(within(box).getByText(/Обслуживание 590/)).toBeTruthy();
    fireEvent.click(within(box).getByRole("button", { name: "Подставить условия" }));
    fireEvent.change(screen.getByLabelText(/Кредитный лимит/), { target: { value: "300000" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(await put.json()).toMatchObject({
      limit_minor: 300_000_00, grace_kind: "windows", window_months: 2, grace_months: 6, opened_on: "2026-07-10",
      grace_all_lost: true, pay_by_period_end: true, charges_in_full: true, annual_rate: "59.99", min_floor_minor: 500_00,
      fees: { cash_free_minor: 100_000_00, cash_percent: "5.9", cash_fixed_minor: 590_00 },
      catalog: { product: "gpb-180-premium", contracts_from: "2025-10-01", revision: "2026-09-10" },
    });
  });

  it("offers the catalog's newer tariff, and applies it only when asked", async () => {
    const base = card();
    answer({
      "/credit-cards/catalog": catalogFixture,
      "/credit-card": {
        ...base,
        terms: { ...base.terms, annual_rate: "49.9", catalog: { product: "gpb-180-premium", contracts_from: "2025-10-01", revision: "2026-01-01" } },
        catalog_update: {
          product: "gpb-180-premium", bank: "Газпромбанк", card: "«180 дней Премиум»", revision: "2026-09-10", sources: [],
          changes: [{ field: "annual_rate", ours: "49.9", theirs: "59.99" }],
        },
      },
    });
    show(<CreditCardPanel account={account} canEdit />);
    const offer = await screen.findByTestId("card-catalog-update");
    expect(norm(offer.textContent ?? "")).toContain("ставка, %: 49.9 → 59.99");
    await waitFor(() => expect(within(offer).getByRole("button", { name: "Применить" })).not.toBeDisabled());
    fireEvent.click(within(offer).getByRole("button", { name: "Применить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(await put.json()).toMatchObject({ annual_rate: "59.99", catalog: { revision: "2026-09-10" } });
  });

  // Fees as Т-Банк and ВТБ state them (#462): the yearly fee's statement, the
  // free transfers of the month, the first days' free part.
  it("tells the yearly fee, the free transfers and the first days", async () => {
    const c = card({ transfers_this_period_minor: 30_000_00, yearly_fee_on: "2027-03-10", intro_left_minor: 20_000_00, intro_until: "2026-09-30" });
    c.terms.fees = { ...noFees, yearly_minor: 590_00, transfer_free_minor: 80_000_00, transfer_percent: "4.9", transfer_fixed_minor: 490_00,
      intro_days: 30, intro_free_minor: 50_000_00, penalty_yearly_percent: "20", penalty_from_day: 6 };
    answer({ "/credit-card": c });
    show(<CreditCardPanel account={account} canEdit={false} />);
    const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");
    expect(norm((await screen.findByTestId("card-yearly-fee")).textContent)).toBe("Обслуживание 590,00 ₽ за год банк спишет с выпиской 10.03.2027.");
    expect(norm(screen.getByTestId("card-transfers").textContent)).toBe("Переводами в этом месяце: 30 000,00 ₽ из бесплатных 80 000,00 ₽.");
    expect(norm(screen.getByTestId("card-intro").textContent)).toBe("До 30.09.2026 снятие и переводы без комиссии — ещё на 20 000,00 ₽.");
    const line = norm(screen.getByTestId("card-panel").textContent);
    expect(line).toContain("обслуживание 590,00 ₽ в год");
    expect(line).toContain("переводы сверх 80 000,00 ₽ в месяц — 4,9 % + 490,00 ₽");
    expect(line).toContain("неустойка 20 % годовых с 6-го дня просрочки");
  });

  it("states the penalty a year and from its day", async () => {
    answer({ "/credit-card": card() });
    Element.prototype.scrollIntoView ??= () => {};
    show(<CreditCardPanel account={account} canEdit />);
    fireEvent.click(await screen.findByRole("button", { name: "Изменить условия" }));
    fireEvent.keyDown(await screen.findByRole("combobox", { name: "Неустойка считается" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "в % годовых" }));
    fireEvent.change(screen.getByLabelText("Неустойка за просрочку, % годовых"), { target: { value: "20" } });
    fireEvent.change(screen.getByLabelText("Неустойка — с какого дня просрочки"), { target: { value: "6" } });
    fireEvent.change(screen.getByLabelText(/Обслуживание в год/), { target: { value: "590" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect((await put.json()).fees).toMatchObject({ penalty_daily_percent: "0", penalty_yearly_percent: "20", penalty_from_day: 6, yearly_minor: 590_00 });
  });

  // Ozon's grace (#460): purchases paid a statement later, «до 140 дней» on
  // Ozon three; and Альфа's «на всё» — cash and transfers in the grace.
  it("states purchases paid statements later and moves in the grace", async () => {
    const c = card();
    c.terms = { ...c.terms, grace_periods: 1, grace_moves: true };
    answer({
      "/credit-card": c,
      "/categories": [{ id: "c-ozon", kind: "expense", name: "Ozon до 140 дней", parent_id: null, archived: false, position: 1 }],
    });
    Element.prototype.scrollIntoView ??= () => {};
    show(<CreditCardPanel account={account} canEdit />);
    const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");
    await waitFor(() => expect(norm(screen.getByTestId("card-panel").textContent)).toContain("покупки месяца — к платежу по выписке через 1, снятие и переводы — тоже в льготе"));
    fireEvent.click(screen.getByRole("button", { name: "Изменить условия" }));
    const box = await screen.findByTestId("card-grace-periods");
    await waitFor(() => expect(within(box).getByTestId("category-select")).toBeTruthy());
    fireEvent.keyDown(within(box).getByRole("combobox"), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "Ozon до 140 дней" }));
    expect((await within(box).findByLabelText("«Ozon до 140 дней» — с выпиской через") as HTMLInputElement).value).toBe("3");
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(await put.json()).toMatchObject({
      grace_periods: 1, grace_moves: true, grace_to_month_end: false, grace_categories: [{ category_id: "c-ozon", periods: 3 }],
    });
  });

  it("names the categories the bank takes for transfers", async () => {
    answer({
      "/credit-card": card(),
      "/categories": [
        { id: "c-food", kind: "expense", name: "Продукты", parent_id: null, archived: false, position: 1 },
        { id: "c-wallets", kind: "expense", name: "Кошельки и ставки", parent_id: null, archived: false, position: 2 },
      ],
    });
    show(<CreditCardPanel account={account} canEdit />);
    fireEvent.click(await screen.findByRole("button", { name: "Изменить условия" }));
    const box = await screen.findByTestId("card-transfer-categories");
    // Radix's select opens on Enter in jsdom, and scrolls to its option.
    Element.prototype.scrollIntoView ??= () => {};
    await waitFor(() => expect(within(box).getByTestId("category-select")).toBeTruthy());
    fireEvent.keyDown(within(box).getByRole("combobox"), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "Кошельки и ставки" }));
    expect(await within(box).findByRole("button", { name: "Убрать «Кошельки и ставки»" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(await put.json()).toMatchObject({ transfer_categories: ["c-wallets"] });
  });

  it("lists the purchases in installments and puts another in", async () => {
    answer({
      "/credit-card": card({
        installments_due_minor: 4_480_00,
        installments: [{
          operation_id: "op-phone", on: "2026-09-10", amount_minor: 12_000_00,
          plan: { months: 3, monthly_fee_percent: "4", fee_minor: 0 },
          billed: 1, left_minor: 8_000_00, next_minor: 4_480_00, note: "Телефон",
        }],
      }),
      "/operations": { operations: [
        { id: "op-tv", account_id: "acc-1", type: "withdrawal", occurred_on: "2026-10-02", amount_minor: -30_000_00, currency: "RUB", note: "Телевизор", counterparty: "", transfer_group_id: null },
        { id: "op-phone", account_id: "acc-1", type: "withdrawal", occurred_on: "2026-09-10", amount_minor: -12_000_00, currency: "RUB", note: "Телефон", counterparty: "", transfer_group_id: null },
      ], has_more: false },
    });
    show(<CreditCardPanel account={account} canEdit />);
    expect(norm((await screen.findByTestId("card-installment")).textContent ?? "")).toContain(
      "10.09.2026 · Телефон — 12 000,00 ₽ на 3 мес.: осталось 8 000,00 ₽, следующая часть 4 480,00 ₽",
    );
    expect(screen.getByTestId("card-installments").textContent).toContain("Части рассрочек к оплате");

    Element.prototype.scrollIntoView ??= () => {};
    fireEvent.click(screen.getByRole("button", { name: "Оформить рассрочку" }));
    const dialog = await screen.findByTestId("card-installment-dialog");
    fireEvent.keyDown(within(dialog).getByRole("combobox", { name: "Покупка" }), { key: "Enter" });
    // The purchase already in installments is not offered again.
    expect(screen.queryByRole("option", { name: /Телефон/ })).toBeNull();
    fireEvent.click(await screen.findByRole("option", { name: /Телевизор/ }));
    fireEvent.change(within(dialog).getByLabelText("Месяцев"), { target: { value: "12" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(new URL(put.url).pathname).toBe("/api/v1/accounts/card-1/credit-card/installments/op-tv");
    expect(await put.json()).toEqual({ months: 12, monthly_fee_percent: "0", fee_minor: 0 });
  });

  it("puts what the bank says over the reckoning, and tells a gap", async () => {
    answer({ "/credit-card": card({
      bank: {
        stated_on: inDays(-2),
        grace: { on: inDays(5), amount_minor: 52_300_00, left_minor: 52_300_00, ours: { on: inDays(5), amount_minor: 52_300_00 }, agrees: true },
        minimum: { on: inDays(11), amount_minor: 1_700_00, left_minor: 1_700_00, ours: { on: inDays(11), amount_minor: 1_569_00 }, agrees: false },
      },
    }) });
    show(<CreditCardPanel account={account} canEdit />);
    expect(norm((await screen.findByTestId("card-minimum")).textContent ?? "")).toBe("1 700,00 ₽");
    expect(screen.getByText("Обязательный платёж (по банку)")).toBeTruthy();
    const bank = norm(screen.getByTestId("card-bank").textContent ?? "");
    expect(bank).toContain("Наш расчёт совпадает.");
    expect(bank).toContain("Наш расчёт: 1 569,00 ₽");
    expect(bank).toContain("Напоминания идут по данным банка");
  });

  it("asks what the bank says", async () => {
    answer({ "/credit-card": card() });
    show(<CreditCardPanel account={account} canEdit />);
    fireEvent.click(await screen.findByRole("button", { name: "Что пишет банк" }));
    const dialog = await screen.findByTestId("card-bank-dialog");
    fireEvent.change(within(dialog).getByLabelText("Обязательный платёж, RUB"), { target: { value: "1700" } });
    fireEvent.change(within(dialog).getByLabelText("Когда посмотрели"), { target: { value: "2026-10-02" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(new URL(put.url).pathname).toBe("/api/v1/accounts/card-1/credit-card/bank");
    expect(await put.json()).toEqual({
      stated_on: "2026-10-02", grace: null, minimum: { on: inDays(11), amount_minor: 1_700_00 },
    });
  });

  it("says which part of the minimum is overdue from before", async () => {
    answer({ "/credit-card": card({ minimum_minor: 1_590_00, minimum_overdue_minor: 500_00 }) });
    show(<CreditCardPanel account={account} canEdit />);
    expect(norm((await screen.findByTestId("card-minimum-overdue")).textContent ?? "")).toBe("в том числе просрочено 500,00 ₽ — внесите сразу");
  });

  it("tells the cashback this month's purchases bring by the card's rules", async () => {
    answer({ "/credit-card": card({ cashback_expected_minor: 1_250_00, cashback_on: "2026-11-06" }) });
    show(<CreditCardPanel account={account} canEdit />);
    expect(norm((await screen.findByTestId("card-cashback")).textContent ?? "")).toBe(
      "Кэшбэк за этот месяц по правилам карты ≈ 1 250,00 ₽, придёт ~06.11.2026.",
    );
  });

  it("says when the grace is off the whole debt, and what brings it back", async () => {
    answer({ "/credit-card": card({
      grace: [], grace_off_since: "2027-01-01", to_restore_minor: 22_385_00,
      lost: [
        { from: "2026-07-01", to: "2026-08-31", deadline: "2026-12-31", amount_minor: 6_385_00, interest_minor: 400_00, early: false },
        { from: "2026-09-01", to: "2026-10-31", deadline: "2027-02-28", amount_minor: 15_000_00, interest_minor: 900_00, early: true },
      ],
    }) });
    show(<CreditCardPanel account={account} canEdit />);
    const off = norm((await screen.findByTestId("card-grace-off")).textContent ?? "");
    expect(off).toContain("С 01.01.2027 льгота снята со всего долга");
    expect(off).toContain("погасите 22 385,00 ₽");
    const lost = screen.getAllByTestId("card-lost").map((e) => norm(e.textContent ?? ""));
    expect(lost[0]).toContain("сгорела");
    expect(lost[1]).toContain("льгота снята досрочно");
  });

  it("says what to pay by when to keep the grace, and the minimum", async () => {
    answer({ "/credit-card": card() });
    show(<CreditCardPanel account={account} canEdit={false} />);
    expect(norm((await screen.findByTestId("card-available")).textContent ?? "")).toBe("92 700,00 ₽");
    const grace = norm(screen.getByTestId("card-grace").textContent ?? "");
    expect(grace).toContain(`внесите 52 300,00 ₽ до ${ru(inDays(5))}`);
    expect(grace).toContain(`Затем 5 000,00 ₽ до ${ru(inDays(36))}`);
    expect(norm(screen.getByTestId("card-minimum").textContent ?? "")).toBe("1 569,00 ₽");
    expect(screen.queryByRole("button", { name: "Изменить условия" })).toBeNull();
  });

  it("warns of a grace lost, and says what a card kept by its balance cannot tell", async () => {
    answer({ "/credit-card": card({ grace: [], lost: [{ from: "2026-09-01", to: "2026-09-30", deadline: "2026-10-21", amount_minor: 42_300_00, interest_minor: 1_400_00, early: false }] }) });
    show(<CreditCardPanel account={account} canEdit />);
    expect(norm((await screen.findByTestId("card-lost")).textContent ?? "")).toMatch(/01\.09\.2026–30\.09\.2026 сгорела: осталось 42 300,00 ₽, проценты уже ≈ 1 400,00 ₽/);
    cleanup();
    answer({ "/credit-card": card({ grace: [] }, false) });
    show(<CreditCardPanel account={account} canEdit />);
    expect(await screen.findByTestId("card-by-balance")).toBeTruthy();
  });
});

describe("CreditCardPanel: the card weighed", () => {
  it("adds what own money earned and the cashback, and takes the bank's charges away", async () => {
    answer({ "/credit-card": card({}, true, {
      from: "2025-10-10", to: "2026-10-10", own_rate_known: true, own_earned_minor: 9_000_00,
      cashback_minor: 5_400_00, costs_minor: 1_290_00, pending_interest_minor: 0, total_minor: 13_110_00,
    }) });
    show(<CreditCardPanel account={account} canEdit />);
    const block = norm((await screen.findByTestId("card-benefit")).textContent ?? "");
    expect(block).toContain("Выгода карты с 10.10.2025+13 110,00 ₽");
    expect(block).toContain("≈ 9 000,00 ₽ (под 15 % годовых)");
    expect(block).toContain("Кэшбэк: 5 400,00 ₽");
    expect(block).toContain("Проценты и комиссии банка: −1 290,00 ₽");
  });

  it("asks for the own money's rate before counting what it earned", async () => {
    answer({ "/credit-card": card({}, true, {
      from: "2026-09-01", to: "2026-10-10", own_rate_known: false, own_earned_minor: 0,
      cashback_minor: 0, costs_minor: 99_00, pending_interest_minor: 1_400_00, total_minor: -1_499_00,
    }) });
    show(<CreditCardPanel account={account} canEdit />);
    const block = norm((await screen.findByTestId("card-benefit")).textContent ?? "");
    expect(block).toContain("Укажите в условиях, сколько приносят ваши деньги");
    expect(norm(screen.getByTestId("card-benefit-pending").textContent ?? "")).toMatch(/≈ −1 400,00 ₽$/);
    expect(norm(screen.getByTestId("card-benefit-total").textContent ?? "")).toBe("-1 499,00 ₽");
  });
});

describe("CardReminders", () => {
  const summary = (over: Partial<CreditCardStatus>) => ({ account_id: "card-1", name: "Кредитка Альфа", currency: "RUB", by_journal: true, status: status(over) });

  it("speaks up a week ahead of the grace's day, and at once for what was missed", async () => {
    answer({ "/credit-cards": [summary({}), { ...summary({ minimum_missed: true, minimum_on: inDays(-2), grace: [] }), account_id: "card-2", name: "Кредитка Сбер" }] });
    show(<CardReminders />);
    const text = norm((await screen.findByTestId("card-reminders")).textContent ?? "");
    expect(text).toContain(`Кредитка Альфа: 52 300,00 ₽ до ${ru(inDays(5))}, чтобы не платить проценты.`);
    expect(text).toContain(`Кредитка Сбер: обязательный платёж 1 569,00 ₽ не внесён до ${ru(inDays(-2))}.`);
  });

  it("stays quiet while nothing is near", async () => {
    answer({ "/credit-cards": [summary({ grace: [{ on: inDays(20), amount_minor: 1_00 }], minimum_on: inDays(20) })] });
    show(<CardReminders />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByTestId("card-reminders")).toBeNull();
  });
});
