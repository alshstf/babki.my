import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { ValuablesPanel } from "./valuables-panel";
import type { AccountWithBalance } from "@/api/accounts";
import type { Valuables, ValuablesItem } from "@/api/valuables";

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

const box: AccountWithBalance = {
  id: "box-1", name: "Ячейка", type: "valuables", currency: "RUB", institution: "", status: "active",
  created_at: "2026-01-01T00:00:00Z", valued_by_balance: false, trades_abroad: false, kept_by_operations: false, foreign_broker: false, tax_owner_user_id: null, counted_by: "journal",
};

const piece = (over: Partial<ValuablesItem>): ValuablesItem => ({
  id: "i-1", kind: "bar", metal: "gold", weight: "100", fineness: "999.9", maker: "Красцветмет", foreign_made: false, serial: "123456",
  certificate: true, sealed: true, bought_on: "2023-03-15", cost_minor: 520_000_00, currency: "RUB", owner_user_id: null, note: "",
  pure_grams: "99.99", market_minor: 1_049_895_00, sell_minor: 99_740_025, sell_by: "guide_percent", buyback_percent: "5.00",
  bank_price_on: null, price_on: "2026-10-12", tax_free_from: "2026-03-15", hard_to_sell: [],
  ...over,
});

// The note's box (Р-39): the 100 g bar free of tax already; a PAMP bar,
// bought lately, with no certificate.
const stored: Valuables = {
  storage: { place: "safe_box", bank: "ВТБ", until: "2099-03-31", fee_minor: 1_800_00, fee_period: "month", buyback_percent: null },
  items: [
    piece({}),
    piece({
      id: "i-2", weight: "10", maker: "PAMP", serial: "A1", foreign_made: true, certificate: false, bought_on: "2025-05-20",
      cost_minor: 95_000_00, market_minor: 104_990_00, sell_minor: 9_449_100, buyback_percent: "10.00", tax_free_from: "2099-05-20",
      hard_to_sell: ["foreign_made", "no_certificate"],
    }),
  ],
  bank_prices: [],
  market_minor: 1_154_885_00,
  sell_minor: 1_091_893_25,
  currency: "RUB",
};

function answer(v: Valuables) {
  fetchMock.mockImplementation((input: Request) => {
    const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
    if (input.url.endsWith("/members")) return json([{ id: "u-1", username: "a", display_name: "Александр", role: "owner" }]);
    return json(v);
  });
}

function show(canEdit = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ValuablesPanel account={box} canEdit={canEdit} />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("ValuablesPanel", () => {
  it("shows each piece with what it would sell for, why, and when free of tax", async () => {
    answer(stored);
    show(false);
    expect(norm((await screen.findByTestId("valuables-sell")).textContent)).toBe("1 091 893,25 ₽");
    expect(norm(screen.getByTestId("valuables-storage").textContent)).toMatch(/Ячейка в банке · ВТБ · до 31\.03\.2099 · 1 800,00 ₽ в месяц/);
    const rows = screen.getAllByTestId("valuables-item");
    expect(norm(rows[0].textContent)).toMatch(/Слиток, золото, 100 г, 999,9Красцветмет · № 123456/);
    expect(norm(rows[0].textContent)).toMatch(/997 400,25 ₽−5 % от цены ЦБ — ориентир программы/);
    expect(norm(rows[0].textContent)).toMatch(/без налога — владеете больше трёх лет/);
    expect(norm(rows[1].textContent)).toMatch(/иностранный заводнет сертификата/);
    expect(norm(rows[1].textContent)).toMatch(/без налога с 20\.05\.2099/);
    expect(screen.getByText(/Ориентир программы — по обзорам/)).toBeTruthy();
    expect(screen.queryByText("Добавить слиток или монету")).toBeNull();
  });

  it("says when the bank's own price stands, and when nothing is priced yet", async () => {
    answer({
      ...stored,
      items: [piece({ sell_by: "bank_price", bank_price_on: "2026-10-01", buyback_percent: null }), piece({ id: "i-3", sell_by: "no_price", sell_minor: null, market_minor: null })],
      bank_prices: [{ metal: "gold", price: "10100", on: "2026-10-01" }],
      market_minor: null,
      sell_minor: null,
    });
    show(false);
    const rows = await screen.findAllByTestId("valuables-item");
    expect(norm(rows[0].textContent)).toMatch(/по цене вашего банка от 01\.10\.2026/);
    expect(norm(rows[1].textContent)).toMatch(/—нет цены ЦБ/);
    expect(screen.getByText(/Не всё оценено/)).toBeTruthy();
    expect(norm(screen.getByTestId("valuables-bank-prices").textContent)).toBe("Цена выкупа банка: золото — 10 100 ₽ за грамм от 01.10.2026");
  });

  it("adds a bar", async () => {
    answer(stored);
    show();
    fireEvent.click(await screen.findByText("Добавить слиток или монету"));
    fireEvent.change(screen.getByLabelText("Вес, г"), { target: { value: "50" } });
    fireEvent.change(screen.getByLabelText("Завод или монетный двор"), { target: { value: "Приокский" } });
    fireEvent.change(screen.getByLabelText("Куплено"), { target: { value: "2024-11-05" } });
    fireEvent.change(screen.getByLabelText("Цена покупки, ₽"), { target: { value: "410 000" } });
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Добавить" }));
    await waitFor(() => {
      const post = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "POST");
      expect(post?.url).toMatch(/\/accounts\/box-1\/valuables\/items$/);
    });
    const post = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "POST")!;
    expect(await post.json()).toMatchObject({
      kind: "bar", metal: "gold", weight: "50", fineness: "999.9", maker: "Приокский", bought_on: "2024-11-05", cost_minor: 410_000_00,
      currency: "RUB", certificate: true, sealed: true, foreign_made: false, owner_user_id: null,
    });
  });
});
