import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { DepositsPage } from "./index";
import type { DepositComparison, DepositComparisonInput } from "@/api/deposit-comparison";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

const salary = {
  id: "salary", name: "Зарплата в ВТБ", kind: "bonus" as const, check: "wages" as const, group: "status", plus: "2", levels: [],
  now_known: false, now_met: false, now_level: 0, measured_minor: null, measured_month: "2026-09", state: "now" as const, chosen_level: -1, met: false, level: 0,
};
const spend = {
  id: "spend", name: "Покупки по картам ВТБ за месяц", kind: "bonus" as const, check: "card_spend" as const, group: null, plus: null,
  levels: [{ from: 2_000_000, plus: "1" }, { from: 5_000_000, plus: "4" }],
  now_known: true, now_met: true, now_level: 0, measured_minor: 3_000_000, measured_month: "2026-09", state: "now" as const, chosen_level: -1, met: true, level: 0,
};

const result: DepositComparison = {
  start_on: "2026-10-19", end_on: "2027-10-19", key_percent: "14", person: "Мария", skipped: 3,
  rows: [
    {
      programme_id: "novikom-kapital-sezonny", programme_name: "Вклад «Капитал Сезонный»", bank_id: "novikom", bank_name: "Новикомбанк", kind: "deposit",
      variant_id: "end", variant_name: "Проценты в конце срока", audience: ["all"], term: 121, term_unit: "days", payout: "end", prolonged: 2, days: 363,
      income_minor: 147_974_00, yearly_percent: "14.88", tax_minor: 1_037_00, net_minor: 146_937_00, insured_minor: 1_000_000_00,
      edition_from: "2026-10-07", sources: ["https://example.ru/n"], conditions: [],
    },
    {
      programme_id: "vtb-schet", programme_name: "Накопительный ВТБ-Счёт", bank_id: "vtb", bank_name: "ВТБ", kind: "savings",
      variant_id: "minimum", variant_name: "На минимальный остаток", audience: ["all"], term: null, term_unit: null, payout: "capitalize", prolonged: 0, days: 365,
      income_minor: 80_000_00, yearly_percent: "8.3", tax_minor: 0, net_minor: 80_000_00, insured_minor: 1_000_000_00,
      edition_from: "2026-09-01", sources: [], conditions: [salary, spend],
    },
  ],
};

function serve() {
  fetchMock.mockImplementation((input: Request) => {
    const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
    const path = new URL(input.url).pathname;
    if (path.endsWith("/auth/me")) return json({ user: { id: "u-maria", username: "m", display_name: "Мария" }, role: "owner", space: { id: "s", name: "S", base_currency: "RUB" } });
    if (path.endsWith("/members")) return json([{ id: "u-maria", username: "m", display_name: "Мария", role: "owner" }]);
    if (path.endsWith("/deposit-comparison")) return json(result);
    return json({});
  });
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <DepositsPage />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

const asked = async (): Promise<DepositComparisonInput[]> => {
  const calls = fetchMock.mock.calls.map(([r]) => r as Request).filter((r) => new URL(r.url).pathname.endsWith("/deposit-comparison"));
  return Promise.all(calls.map((r) => r.clone().json()));
};

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("DepositsPage", () => {
  it("compares on a million for a year and lists the rows by what is left", async () => {
    serve();
    show();
    const rows = await screen.findAllByTestId("deposits-row");
    expect(norm(rows[0].textContent)).toMatch(/Новикомбанк · Вклад «Капитал Сезонный»Проценты в конце срока · 121 дн\. × 3, проценты в конце/);
    expect(norm(rows[0].textContent)).toMatch(/14,88 %.*146 937,00 ₽/);
    expect(norm(rows[1].textContent)).toMatch(/На минимальный остаток · накопительный · условий выполнено: 1 из 2/);
    const first = (await asked())[0];
    expect(first).toMatchObject({ amount_minor: 100_000_000, months: 12, preset: "now", key_percent: null, choices: [], audiences: [] });
    expect(norm(screen.getByTestId("deposits-notes").textContent)).toMatch(/Ключевая ставка 14 % — на весь срок/);
    expect(norm(screen.getByTestId("deposits-notes").textContent)).toMatch(/Не вошли: 3/);
  });

  it("opens a row's conditions and takes a choice of one of them", async () => {
    serve();
    show();
    const rows = await screen.findAllByTestId("deposits-row");
    fireEvent.click(rows[1]);
    const conditions = await screen.findByTestId("deposits-conditions");
    expect(norm(conditions.textContent)).toMatch(/Зарплата в ВТБ · \+2сейчас: по данным программы не видно/);
    expect(norm(conditions.textContent)).toMatch(/сейчас: выполняется — 30 000,00 ₽ за прошлый месяц/);
    fireEvent.click(within(conditions).getByLabelText("Зарплата в ВТБ"));
    fireEvent.click(await screen.findByRole("option", { name: "выполняется" }));
    await waitFor(async () => {
      const all = await asked();
      expect(all[all.length - 1].choices).toEqual([{ programme_id: "vtb-schet", variant_id: "minimum", condition: "salary", state: "yes", level: null }]);
    });
  });
});
