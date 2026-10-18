import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { RatePanel } from "./rate-panel";
import type { AccountWithBalance } from "@/api/accounts";
import type { AccountRates } from "@/api/account-rates";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

const deposit: AccountWithBalance = {
  id: "dep-1", name: "Вклад", type: "deposit", currency: "RUB", institution: "", status: "active",
  created_at: "2026-01-01T00:00:00Z", valued_by_balance: false, trades_abroad: false, kept_by_operations: false, foreign_broker: false, tax_owner_user_id: null, counted_by: "balance",
};

// Decision Р-51 А: 16 % from January, the key rate less 1.5 from September.
const bare = { source: "none" as const, programme: null, quote: null, conditions: [], marks: [] };

const stated: AccountRates = {
  ...bare,
  source: "by_hand",
  rates: [
    { from_on: "2026-01-01", until_on: null, kind: "fixed", percent: "16.000", by_hand: true },
    { from_on: "2026-09-01", until_on: null, kind: "key", percent: "-1.500", by_hand: true },
  ],
  current: { percent: "15.500", from_on: "2026-09-01", until_on: null, kind: "key", by_hand: true },
  key_rate: "17",
};

// The catalog's ВТБ-Счёт, Газпромбанк's deposit and one in yuan, for the
// dialog.
const catalog = {
  programmes: [
    {
      id: "gpb-km", bank_id: "gazprombank", bank_name: "Газпромбанк", name: "Вклад «Ключевой момент»", kind: "deposit", edition_from: "2026-10-03",
      checked_on: "2026-10-16", sources: ["https://example.ru/gpb"], notes: "",
      variants: [{
        id: "end", name: "Проценты в конце срока", currency: "RUB", accrual: null, payouts: ["end"], ops: "none", term_unit: "days",
        terms: [{ from: 181, to: 181 }, { from: 367, to: 367 }], amount_from_minor: 100_000_00, conditions: [], welcome: null,
      }],
    },
    {
      id: "gpb-cny", bank_id: "gazprombank", bank_name: "Газпромбанк", name: "Вклад «Расширяй возможности»", kind: "deposit", edition_from: "2026-10-03",
      checked_on: "2026-10-16", sources: [], notes: "",
      variants: [{
        id: "end", name: "В юанях", currency: "CNY", accrual: null, payouts: ["end"], ops: "none", term_unit: "days",
        terms: [{ from: 91, to: 91 }], amount_from_minor: 5_000_00, conditions: [], welcome: null,
      }],
    },
    {
      id: "sber-sv", bank_id: "sber", bank_name: "Сбербанк", name: "СберВклад", kind: "deposit", edition_from: "2026-10-16",
      checked_on: "2026-10-16", sources: [], notes: "",
      variants: [{
        id: "monthly", name: "Онлайн, проценты ежемесячно", currency: "RUB", accrual: null, payouts: ["monthly", "capitalize"], ops: "none", term_unit: "months",
        terms: [{ from: 6, to: 6 }], amount_from_minor: 100_000_00, conditions: [], welcome: null,
      }],
    },
    {
      id: "vtb-schet", bank_id: "vtb", bank_name: "ВТБ", name: "Накопительный ВТБ-Счёт", kind: "savings", edition_from: "2026-10-16",
      checked_on: "2026-10-16", sources: [], notes: "", variants: [],
    },
  ],
};

function answer(r: AccountRates) {
  fetchMock.mockImplementation((input: Request) => {
    const body = new URL(input.url).pathname === "/api/v1/deposit-programmes" ? catalog : r;
    return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
  });
}

const sent = async (method: string, path: string) => {
  await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === method && new URL((r as Request).url).pathname === path)).toBe(true));
  return fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === method && new URL(r.url).pathname === path)!;
};

// The ВТБ-Счёт of the note (Р-53): 12 % by the bank's terms — 6 base, 2 for
// the wages, 4 for purchases from 50 000 ₽, on up to 1 млн; 6 % above it.
const tied: AccountRates = {
  ...bare,
  source: "programme",
  key_rate: "14",
  rates: [],
  current: null,
  programme: {
    programme_id: "vtb-schet", programme_name: "Накопительный ВТБ-Счёт", bank_id: "vtb", bank_name: "ВТБ", kind: "savings",
    variant_id: "minimum", variant_name: "На минимальный остаток", opened_on: "2026-04-01", term: null, term_unit: null, amount_minor: null, conditions: "manual", payout: null,
  },
  quote: {
    percent: "11.000", edition_from: "2026-10-16", checked_on: "2026-10-16", sources: [], notes: "", period: null, bonuses: ["salary", "spend"], mgs: "6",
    parts: [{ from_minor: 0, to_minor: 1_000_000_00, percent: "12.000" }, { from_minor: 1_000_000_00, to_minor: 1_200_000_00, percent: "6.000" }],
  },
  conditions: [
    { id: "salary", name: "Зарплата в ВТБ", kind: "bonus", check: "wages", group: "status", plus: "2", levels: [], met: true, level: 0, told: "mark", measured_minor: null, measured_month: null },
    { id: "spend", name: "Покупки по картам ВТБ за месяц", kind: "bonus", check: "card_spend", group: null, plus: null, levels: [{ from: 2_000_000, plus: "1" }, { from: 5_000_000, plus: "4" }], met: true, level: 1, told: "mark", measured_minor: null, measured_month: null },
  ],
};

function show(canEdit = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <RatePanel account={deposit} canEdit={canEdit} />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("RatePanel", () => {
  it("shows the rate in force, how it is struck, that it is by hand, and the ones before", async () => {
    answer(stated);
    show();
    expect(norm((await screen.findByTestId("account-rate-now")).textContent)).toBe("15,5 % годовых");
    const panel = norm(screen.getByTestId("account-rate").textContent);
    expect(panel).toContain("ключевая 17 % − 1,5 % · с 01.09.2026 · указана вручную");
    const rows = within(screen.getByTestId("account-rate-history")).getAllByRole("listitem").map((li) => norm(li.textContent));
    expect(rows).toEqual(["с 01.09.2026 — ключевая ставка ЦБ − 1,5 % · вручную", "с 01.01.2026 — 16 % годовых · вручную"]);
  });

  it("says what a goal grows at without a rate, and offers one only to an editor", async () => {
    answer({ ...bare, rates: [], current: null, key_rate: "17" });
    show(false);
    expect(await screen.findByText(/Ставки пока нет/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Указать ставку" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Выбрать программу банка" })).toBeNull();
  });

  it("states a rate tied to the key rate, showing what it comes to", async () => {
    answer({ ...bare, rates: [], current: null, key_rate: "17" });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Указать ставку" }));
    const dialog = await screen.findByTestId("account-rate-dialog");
    fireEvent.click(within(dialog).getByRole("combobox"));
    fireEvent.click(await screen.findByRole("option", { name: "Ключевая ставка ЦБ плюс или минус" }));
    fireEvent.change(within(dialog).getByLabelText("Плюс или минус к ключевой, %"), { target: { value: "−1,5" } });
    fireEvent.change(within(dialog).getByLabelText("Действует с"), { target: { value: "2026-09-01" } });
    expect(norm(dialog.textContent)).toContain("Сейчас ключевая 17 % — по счёту выйдет 15,5 % годовых.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(new URL(put.url).pathname).toBe("/api/v1/accounts/dep-1/rates");
    expect(await put.json()).toEqual({ from_on: "2026-09-01", until_on: null, kind: "key", percent: "-1.5" });
  });

  // Р-59 А: a rate for the months before the bank's programme is known, to
  // its last day; a last day before the first is not saved.
  it("states a rate to its last day", async () => {
    answer({ ...bare, rates: [], current: null, key_rate: "17" });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Указать ставку" }));
    const dialog = await screen.findByTestId("account-rate-dialog");
    fireEvent.change(within(dialog).getByLabelText("Ставка, % годовых"), { target: { value: "18" } });
    fireEvent.change(within(dialog).getByLabelText("Действует с"), { target: { value: "2026-01-01" } });
    fireEvent.change(within(dialog).getByLabelText("Действует по (необязательно)"), { target: { value: "2025-12-31" } });
    expect(dialog.textContent).toContain("Последний день не раньше первого.");
    expect((within(dialog).getByRole("button", { name: "Сохранить" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(within(dialog).getByLabelText("Действует по (необязательно)"), { target: { value: "2026-09-30" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    const put = await sent("PUT", "/api/v1/accounts/dep-1/rates");
    expect(await put.json()).toEqual({ from_on: "2026-01-01", until_on: "2026-09-30", kind: "fixed", percent: "18" });
  });

  it("shows a rate's last day in the history and under the rate", async () => {
    answer({
      ...stated,
      rates: [{ from_on: "2026-01-01", until_on: "2099-03-31", kind: "fixed", percent: "18.000", by_hand: true }],
      current: { percent: "18.000", from_on: "2026-01-01", until_on: "2099-03-31", kind: "fixed", by_hand: true },
    });
    show();
    expect(norm((await screen.findByTestId("account-rate-history")).textContent)).toContain("с 01.01.2026 по 31.03.2099 — 18 % годовых");
    expect(norm(screen.getByTestId("account-rate").textContent)).toContain("с 01.01.2026 по 31.03.2099 · указана вручную");
  });

  it("does not wait for the key rate on a key-tied rate that has ended", async () => {
    answer({
      ...bare,
      rates: [{ from_on: "2026-01-01", until_on: "2026-03-31", kind: "key", percent: "-1.000", by_hand: true }],
      current: null,
      key_rate: null,
    });
    show();
    expect(norm((await screen.findByTestId("account-rate")).textContent)).toContain("Ставки пока нет");
  });

  it("shows the rate by the bank's terms: the bonuses that counted, the parts of the balance, the marks", async () => {
    answer(tied);
    show();
    expect(norm((await screen.findByTestId("account-rate-now")).textContent)).toBe("11 % годовых");
    const quote = norm(screen.getByTestId("account-rate-quote").textContent);
    expect(quote).toContain("по тарифу: ВТБ · Накопительный ВТБ-Счёт · На минимальный остаток · редакция с 16.10.2026");
    expect(quote).toContain("Надбавки: Зарплата в ВТБ, Покупки по картам ВТБ за месяц");
    expect(quote).toContain("на 0,00 ₽ – 1 000 000,00 ₽: 12 % · на 1 000 000,00 ₽ – 1 200 000,00 ₽: 6 %");
    const block = norm(screen.getByTestId("account-programme").textContent);
    expect(block).toContain("На минимальный остаток · открыт 01.04.2026");
    expect((screen.getByRole("checkbox", { name: /Зарплата в ВТБ/ }) as HTMLButtonElement).getAttribute("data-state")).toBe("checked");
    fireEvent.click(screen.getByRole("checkbox", { name: /Зарплата в ВТБ/ }));
    const put = await sent("PUT", "/api/v1/accounts/dep-1/marks");
    const body = await put.json();
    expect(body).toMatchObject({ condition: "salary", level: 0, met: false });
    expect(body.from_on).toMatch(/^\d{4}-\d{2}-01$/);
  });

  it("ties a deposit to its bank's programme: the term and sum the bank has a rate for", async () => {
    answer({ ...bare, rates: [], current: null, key_rate: "14" });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Выбрать программу банка" }));
    const dialog = await screen.findByTestId("account-programme-dialog");
    fireEvent.click(await within(dialog).findByRole("combobox"));
    fireEvent.click(await screen.findByRole("option", { name: "Газпромбанк · Вклад «Ключевой момент»" }));
    expect(screen.queryByRole("option", { name: /ВТБ-Счёт/ })).toBeNull(); // a savings account's programme is not offered
    expect(screen.queryByRole("option", { name: /Расширяй возможности/ })).toBeNull(); // nor one in yuan to a rouble deposit
    fireEvent.change(within(dialog).getByLabelText("Когда открыт или продлён вклад"), { target: { value: "2026-10-05" } });
    const save = within(dialog).getByRole("button", { name: "Сохранить" });
    fireEvent.change(within(dialog).getByLabelText("Срок, дн."), { target: { value: "366" } });
    fireEvent.change(within(dialog).getByLabelText("Сумма при открытии, RUB"), { target: { value: "500 000" } });
    expect((save as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(within(dialog).getByLabelText("Срок, дн."), { target: { value: "367" } });
    fireEvent.click(save);
    const put = await sent("PUT", "/api/v1/accounts/dep-1/programme");
    expect(await put.json()).toEqual({
      programme_id: "gpb-km", variant_id: "end", opened_on: "2026-10-05", term: 367, amount_minor: 50_000_000, conditions: "auto", payout: null,
    });
    // One way of payment: nothing asked.
    expect(within(dialog).queryByLabelText("Как платятся проценты")).toBeNull();
  });

  it("asks how a deposit pays its interest where its variant has more than one way", async () => {
    answer({ ...bare, rates: [], current: null, key_rate: "14" });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Выбрать программу банка" }));
    const dialog = await screen.findByTestId("account-programme-dialog");
    fireEvent.click(await within(dialog).findByRole("combobox"));
    fireEvent.click(await screen.findByRole("option", { name: "Сбербанк · СберВклад" }));
    fireEvent.change(within(dialog).getByLabelText("Когда открыт или продлён вклад"), { target: { value: "2026-10-16" } });
    fireEvent.change(within(dialog).getByLabelText("Срок, мес."), { target: { value: "6" } });
    fireEvent.change(within(dialog).getByLabelText("Сумма при открытии, RUB"), { target: { value: "500 000" } });
    fireEvent.click(within(dialog).getByLabelText("Как платятся проценты"));
    fireEvent.click(await screen.findByRole("option", { name: "каждый месяц в сумму вклада" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    const put = await sent("PUT", "/api/v1/accounts/dep-1/programme");
    expect(await put.json()).toEqual({
      programme_id: "sber-sv", variant_id: "monthly", opened_on: "2026-10-16", term: 6, amount_minor: 50_000_000, conditions: "auto", payout: "capitalize",
    });
  });

  it("in the automatic mode says what the journals told, and gives a mark back to them", async () => {
    answer({
      ...tied,
      programme: { ...tied.programme!, conditions: "auto" },
      marks: [{ condition: "salary", from_on: "2026-10-01", level: 0, met: true }],
      conditions: [
        { ...tied.conditions[0], told: "mark" },
        { ...tied.conditions[1], told: "data", measured_minor: 5_520_000, measured_month: "2026-09" },
        { id: "level", name: "Уровень «Расти с ВТБ»", kind: "bonus", check: "manual", group: null, plus: null, levels: [{ from: 1, plus: "1" }], met: false, level: 0, told: "none", measured_minor: null, measured_month: null },
      ],
    });
    show();
    const block = norm((await screen.findByTestId("account-programme")).textContent);
    expect(block).toContain("Условия проверяет программа по операциям и остаткам");
    expect(block).toContain("по операциям за сентябрь 2026: покупки по картам банка 55 200,00 ₽");
    expect(block).toContain("отмечено вами — главнее проверки.");
    expect(block).not.toContain("нет операций"); // a condition only a mark tells is not «no data»
    fireEvent.click(screen.getByRole("button", { name: "Вернуть проверку программой" }));
    await sent("DELETE", "/api/v1/accounts/dep-1/marks/salary/2026-10-01");
  });
});
