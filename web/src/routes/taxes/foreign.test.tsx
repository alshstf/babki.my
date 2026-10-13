import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { ForeignReturn } from "./foreign";
import type { TaxYearForeign } from "@/api/tax-year";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Alex's Freedom in 2026 (decision Р-45): an Apple dividend with tax withheld
// in the United States, a Eurobond coupon, the broker's interest with no rate,
// and a sale of two lots.
const foreign: TaxYearForeign = {
  year: 2026, supported: true, currency: "RUB",
  accounts: [{
    account_id: "a-kz", name: "Freedom KZ", user_id: "u-alex", country: "KZ", country_code: "398",
    payouts: [
      { on: "2026-05-15", type: "dividend", kind: "001", instrument_id: "i-aapl", paper: "Apple", isin: "US0378331005", country: "US", country_code: "840",
        currency: "USD", currency_code: "840", amount_minor: 1000, withheld_minor: 300, rate: "85", amount_base_minor: 85_000, withheld_base_minor: 25_500,
        tax_ru_minor: 11_100, credit_minor: 11_100 },
      { on: "2026-06-01", type: "coupon", kind: "003", instrument_id: "i-xs", paper: "Евробонд", isin: "XS1234567890", country: "XS", country_code: "",
        currency: "USD", currency_code: "840", amount_minor: 500, withheld_minor: 0, rate: "84.5", amount_base_minor: 42_250, withheld_base_minor: 0,
        tax_ru_minor: 5_500, credit_minor: 0 },
      { on: "2026-06-30", type: "interest", kind: "013", instrument_id: null, paper: "", isin: "", country: "", country_code: "",
        currency: "USD", currency_code: "840", amount_minor: 200, withheld_minor: 0, rate: null, amount_base_minor: 0, withheld_base_minor: 0,
        tax_ru_minor: 0, credit_minor: 0 },
    ],
    sales: [{
      on: "2026-02-10", settled_on: "2026-02-11", instrument_id: "i-aapl", paper: "Apple", isin: "US0378331005", quantity: "12", currency: "USD", currency_code: "840",
      proceeds_minor: 180_000, fee_minor: 200, rate: "80", proceeds_base_minor: 14_400_000, fee_base_minor: 16_000,
      lots: [
        { acquired_on: "2022-03-01", quantity: "10", currency: "USD", cost_minor: 100_000, rate: "90", cost_base_minor: 9_000_000 },
        { acquired_on: "2025-04-01", quantity: "2", currency: "USD", cost_minor: 24_000, rate: "100", cost_base_minor: 2_400_000 },
      ],
      result_base_minor: 2_984_000, long_base_minor: 2_986_667,
    }],
    other_taxes: [],
    income_base_minor: 14_400_000, expenses_base_minor: 11_416_000, unvalued: 1,
  }],
};

function show(body: TaxYearForeign) {
  fetchMock.mockImplementation((input: Request) => {
    const json = (b: unknown) => Promise.resolve(new Response(JSON.stringify(b), { status: 200, headers: { "Content-Type": "application/json" } }));
    if (new URL(input.url).pathname.endsWith("/members")) return json([{ id: "u-alex", username: "a", display_name: "Алекс", role: "owner" }]);
    return json(body);
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ForeignReturn year={2026} />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("ForeignReturn", () => {
  it("lays out the payments and sales as the return asks", async () => {
    show(foreign);
    const account = await screen.findByTestId("tax-foreign-account");
    expect(norm(account.textContent)).toMatch(/Алекс · брокер: Казахстан \(398\)/);
    const payouts = within(account).getAllByTestId("tax-foreign-payout").map((r) => norm(r.textContent));
    expect(payouts[0]).toMatch(/15\.05\.2026Apple001Соединенные Штаты \(840\)10,00 \$85850,00 ₽255,00 ₽111,00 ₽/);
    expect(payouts[1]).toMatch(/Евробонд003.*84,5422,50 ₽——/);
    expect(payouts[2]).toMatch(/Проценты брокера013—2,00 \$———/);
    const sale = norm(within(account).getByTestId("tax-foreign-sale").textContent);
    expect(sale).toMatch(/Appleштук: 121 800,00 \$по курсу 80 на 11\.02\.2026 — 144 000,00 ₽комиссия 160,00 ₽/);
    expect(sale).toMatch(/01\.03\.2022 · 10 шт\. · курс 90 · 90 000,00 ₽/);
    expect(sale).toMatch(/29 840,00 ₽из них 29 866,67 ₽ — по бумагам дольше трёх лет/);
    expect(norm(within(account).getByTestId("tax-foreign-app8").textContent)).toBe(
      "Приложение 8: доход по продажам (строка 020) — 144 000,00 ₽, расходы (строка 030) — 114 160,00 ₽.",
    );
    expect(norm(account.textContent)).toMatch(/Без курса или даты покупки и не вошли в суммы строк: 1\./);
  });

  it("saves the lines as a table with the return's codes", async () => {
    const blobs: Blob[] = [];
    const create = vi.fn((b: Blob) => {
      blobs.push(b);
      return "blob:x";
    });
    URL.createObjectURL = create as unknown as typeof URL.createObjectURL;
    URL.revokeObjectURL = vi.fn();
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    show(foreign);
    fireEvent.click(await screen.findByRole("button", { name: "Скачать таблицу" }));
    expect(click).toHaveBeenCalled();
    const text = await blobs[0].text();
    const lines = text.replace(/^﻿/, "").split("\r\n");
    expect(lines[2]).toBe("15.05.2026;Apple;001;840;398;840;10,00;85;850,00;3,00;255,00;111,00;111,00");
    expect(lines).toContain("10.02.2026;Apple;US0378331005;12;840;1800,00;2,00;11.02.2026;80;144000,00;160,00;01.03.2022;10;1000,00;90;90000,00");
    expect(lines).toContain("10.02.2026;Apple;US0378331005;12;840;;;11.02.2026;80;;;01.04.2025;2;240,00;100;24000,00");
    expect(lines.at(-1)).toBe("Приложение 8, строка 030 — расходы;114160,00");
    click.mockRestore();
  });

  it("is not there without accounts abroad", async () => {
    show({ ...foreign, accounts: [] });
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByTestId("tax-foreign")).toBeNull();
  });
});
