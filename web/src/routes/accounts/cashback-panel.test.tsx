import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { CashbackPanel } from "./cashback-panel";
import type { AccountWithBalance } from "@/api/accounts";
import type { CardCashback } from "@/api/cashback";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}
if (!Element.prototype.hasPointerCapture) {
  Element.prototype.hasPointerCapture = () => false;
}

const card: AccountWithBalance = {
  id: "card-1", name: "Карта ВТБ", type: "checking", currency: "RUB", institution: "", status: "active",
  created_at: "2026-01-01T00:00:00Z", valued_by_balance: false, trades_abroad: false, kept_by_operations: false, foreign_broker: false, tax_owner_user_id: null, counted_by: "journal",
};

const untied: CardCashback = { programme: null, period_from: null, period_to: null, lines: [], bonus_minor: 0, capped: false, uncoded_minor: 0, uncoded_count: 0 };
const programme = {
  programme_id: "vtb-multibonus", programme_name: "Мультибонус", bank_name: "ВТБ", variant_id: "multicard", variant_name: "«Мультикарта»",
  pick: 3, of: 8, cap_minor: 0, period: "month", edition_from: "2026-10-01", sources: [], notes: "", known: true,
  categories: [
    { id: "supermarkets", name: "Супермаркеты", by_merchant: false },
    { id: "pharmacy", name: "Аптеки", by_merchant: false },
    { id: "fuel", name: "АЗС", by_merchant: false },
  ],
};
const october: CardCashback = {
  programme, period_from: "2026-10-01", period_to: "2026-10-31",
  lines: [
    { category_id: "fuel", name: "АЗС", percent: "7", spent_minor: 0, bonus_minor: 0, purchases: 0 },
    { category_id: "supermarkets", name: "Супермаркеты", percent: "5", spent_minor: 1_000_000, bonus_minor: 50_000, purchases: 1 },
  ],
  bonus_minor: 50_000, capped: false, uncoded_minor: 30_000, uncoded_count: 1,
};

function answer(data: CardCashback) {
  fetchMock.mockImplementation(async (input: Request) => {
    const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    const path = new URL(input.url).pathname;
    if (path === "/api/v1/loyalty-programmes") {
      return json([{ id: "vtb-multibonus", bank_id: "vtb", bank_name: "ВТБ", name: "Мультибонус", edition_from: "2026-10-01", period: "month", sources: [], notes: "", categories: programme.categories, variants: [{ id: "multicard", name: "«Мультикарта»", pick: 3, of: 8, cap_minor: 0 }] }]);
    }
    return json(data);
  });
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <CashbackPanel account={card} canEdit />
    </QueryClientProvider>,
  );
}

const sent = async (method: string, path: string) => {
  const find = () => fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === method && new URL(r.url).pathname === path);
  await waitFor(() => expect(find()).toBeTruthy());
  return find()!;
};

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

// Decision Р-58.
describe("CashbackPanel", () => {
  it("ties a card to a programme and its variant", async () => {
    answer(untied);
    show();
    expect(await screen.findByText(/Карта не привязана к программе лояльности банка/)).toBeTruthy();
    fireEvent.click(await screen.findByRole("combobox", { name: "Программа лояльности" }));
    fireEvent.click(await screen.findByRole("option", { name: "ВТБ · Мультибонус" }));
    fireEvent.click(screen.getByRole("combobox", { name: "Карта или пакет" }));
    fireEvent.click(await screen.findByRole("option", { name: "«Мультикарта»" }));
    fireEvent.click(screen.getByRole("button", { name: "Привязать" }));
    expect(await (await sent("PUT", "/api/v1/accounts/card-1/cashback/programme")).json()).toEqual({ programme_id: "vtb-multibonus", variant_id: "multicard" });
  });

  it("shows the period's categories, what each earns, and the purchases without a code", async () => {
    answer(october);
    show();
    expect(norm((await screen.findByTestId("cashback-period")).textContent)).toBe("Период с 01.10.2026 по 31.10.2026");
    const lines = within(screen.getByTestId("cashback-lines")).getAllByRole("listitem").map((l) => norm(l.textContent));
    expect(lines[1]).toBe("Супермаркеты — 5 % с покупок на 10 000,00 ₽500,00 ₽");
    expect(norm(screen.getByTestId("cashback-total").textContent)).toBe("Кэшбэк за период ≈ 500,00 ₽");
    expect(norm(screen.getByTestId("cashback").textContent)).toContain("Покупок без кода: 1 на 300,00 ₽");
  });

  it("asks for the period's picks and their percents", async () => {
    answer({ ...october, lines: [], bonus_minor: 0 });
    show();
    expect(await screen.findByTestId("cashback-none")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Указать категории" }));
    const dialog = await screen.findByTestId("cashback-picks");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Аптеки" }));
    fireEvent.change(within(dialog).getByLabelText("Процент: Аптеки"), { target: { value: "3,5" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    expect(await (await sent("PUT", "/api/v1/accounts/card-1/cashback/picks")).json()).toEqual({ on: "2026-10-01", picks: [{ category_id: "pharmacy", percent: "3.5" }] });
  });
});
