import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { PlanCard } from "./plan";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

const account = (id: string, name: string, type: string) => ({
  id, name, type, currency: "RUB", institution: "", status: "active", created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false, trades_abroad: false, kept_by_operations: true, foreign_broker: false, tax_owner_user_id: null,
  counted_by: "journal", owner_user_id: null, balance: null,
});
const qqqm = { id: "i-qqqm", type: "etf", name: "Invesco NASDAQ 100", ticker: "QQQM", isin: "US46138G6492", figi: "", currency: "USD", frozen: false, face_value_minor: null, face_currency: null };

// The owner's example (Р-48): $1 000 of QQQM at the broker — funds closer to
// their target, the United States further above its ceiling, currencies
// unmoved; and the roubles at the broker run short.
const answer = {
  base_currency: "RUB",
  cuts: [
    { dimension: "kind", rows: [
      { key: "funds", before_minor: 880_000_00, after_minor: 970_000_00, share_before: "22.00", share_after: "24.25", target: "25.00", verdict: "closer" },
      { key: "broker_cash", before_minor: 50_000_00, after_minor: -40_000_00, share_before: "1.25", share_after: "-1.00", target: null, verdict: "none" },
      { key: "bonds", before_minor: 1_000_000_00, after_minor: 1_000_000_00, share_before: "25.00", share_after: "25.00", target: "30.00", verdict: "same" },
    ] },
    { dimension: "currency", rows: [{ key: "USD", before_minor: 0, after_minor: 0, share_before: "0.00", share_after: "0.00", target: null, verdict: "none" }] },
    { dimension: "country", rows: [{ key: "US", before_minor: 750_000_00, after_minor: 840_000_00, share_before: "25.00", share_after: "27.25", target: "20.00", verdict: "further" }] },
    { dimension: "sector", rows: [] },
  ],
  uninsured: [{ user_id: "u-maria", bank: "tbank", before_minor: 0, after_minor: 400_000_00 }],
  sales: [], tax_minor: null, tax_rate: null, missing_rates: [],
};

// The rebalance's suggestion: sell 280 000 of shares, buy as much of bonds;
// the deposits' part above target is to be taken as they end.
const rebalance = {
  amount_minor: 0, worth_after_minor: 4_000_000_00, sold_minor: 280_000_00, from_brokers_minor: 0, bought_minor: 280_000_00,
  kinds: [
    { key: "shares", change_minor: -280_000_00, later_minor: 0, kept_minor: 0, share_after: "30.00", sale: null },
    { key: "bonds", change_minor: 280_000_00, later_minor: 0, kept_minor: 0, share_after: "30.00", sale: null },
    { key: "deposits", change_minor: 0, later_minor: 100_000_00, kept_minor: 0, share_after: "20.00", sale: null },
  ],
  tax_minor: null, tax_rate: null, long_holding_years: 3,
};

const october = {
  id: "p-oct", name: "Октябрь", saved_at: "2026-10-13T18:00:00Z",
  operations: [{ kind: "buy", account_id: "a-broker", to_account_id: null, instrument_id: "i-qqqm", currency: "USD", amount_minor: 1000_00 }],
};

// brokerCurrency: the broker's account's; the family's base is the rouble.
function show(role = "viewer", plans: unknown[] = [], brokerCurrency = "RUB") {
  fetchMock.mockImplementation((input: Request) => {
    const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
    const url = new URL(input.url);
    if (url.pathname.endsWith("/auth/me")) return json({ user: { id: "u", username: "a", display_name: "A" }, role, space_id: "s", space_name: "S", base_currency: "RUB" });
    if (url.pathname.endsWith("/allocation/plans")) return json({ plans });
    if (url.pathname.endsWith("/allocation/rebalance")) return json(rebalance);
    if (url.pathname.endsWith("/accounts")) return json([account("a-card", "Карта", "checking"), { ...account("a-broker", "Freedom", "brokerage"), currency: brokerCurrency }]);
    if (url.pathname.endsWith("/instruments")) return json({ instruments: [qqqm], has_more: false });
    if (url.pathname.endsWith("/what-if")) return json(answer);
    if (url.pathname.endsWith("/members")) return json([{ id: "u-maria", username: "m", display_name: "Мария", role: "editor" }]);
    if (url.pathname.endsWith("/intermediaries")) return json({ checked_on: "2026-10-14", intermediaries: [{ id: "tbank", name: "Т-Банк", kind: "bank", country: "RU", insured: true, reg_number: 2673, bank_id: null, aliases: [] }] });
    return json(null);
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <PlanCard valuation="liquid" />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("PlanCard", () => {
  it("lays a purchase against the targets and ceilings", async () => {
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Добавить операцию" }));
    const form = screen.getByTestId("plan-form");
    fireEvent.click(await within(form).findByRole("button", { name: /Invesco NASDAQ 100/ }));
    fireEvent.change(within(form).getByLabelText("Сумма"), { target: { value: "1000" } });
    expect((within(form).getByLabelText("Валюта") as HTMLInputElement).value).toBe("USD");
    fireEvent.click(within(form).getByRole("button", { name: "Добавить в план" }));

    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => new URL((r as Request).url).pathname.endsWith("/what-if"))).toBe(true));
    const post = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => new URL(r.url).pathname.endsWith("/what-if"))!;
    expect(await post.json()).toEqual({
      operations: [{ kind: "buy", account_id: "a-broker", to_account_id: null, instrument_id: "i-qqqm", currency: "USD", amount_minor: 1000_00 }],
    });
    expect(norm(screen.getByTestId("plan-ops").textContent)).toMatch(/Freedom: купить Invesco NASDAQ 100 на 1 000,00 \$/);
    const result = await screen.findByTestId("plan-result");
    expect(norm(within(result).getByTestId("plan-cut-kind").textContent)).toMatch(/Фонды.*ближе к цели22 % → 24,25 % · цель 25 %/);
    expect(within(result).getByTestId("plan-cut-kind").textContent).not.toMatch(/Облигации/);
    expect(within(result).queryByTestId("plan-cut-currency")).toBeNull();
    expect(norm(within(result).getByTestId("plan-cut-country").textContent)).toMatch(/дальше от цели25 % → 27,25 % · потолок 20 %/);
    expect(norm(result.textContent)).toMatch(/Денег у брокеров не хватит: не хватает 40\s000/);
    await waitFor(() => expect(norm(within(result).getByTestId("plan-uninsured").textContent)).toMatch(/Мария · Т-Банк: сверх страховки вкладов станет 400\s000 ₽/));
    fireEvent.click(within(result).getByRole("button", { name: "Показать все разрезы" }));
    expect(within(result).getByTestId("plan-cut-kind").textContent).toMatch(/Облигации.*без изменений/);
  });

  it("fills the plan from the suggestion, sales first, the sum kept in roubles", async () => {
    show("viewer", [], "USD");
    fireEvent.click(await screen.findByRole("button", { name: "Заполнить из подсказки" }));
    const pending = await screen.findByTestId("plan-pending");
    const lines = within(pending).getAllByRole("listitem").map((l) => norm(l.textContent));
    expect(lines).toHaveLength(2);
    expect(lines[0]).toMatch(/^Продать: Акции и расписки на 280 000 ₽ — выберите счёт и бумагу/);
    expect(lines[1]).toMatch(/^Купить: Облигации на 280 000 ₽/);
    fireEvent.click(within(pending).getAllByRole("button", { name: "выбрать бумагу" })[1]);
    const form = screen.getByTestId("plan-form");
    expect((within(form).getByLabelText("Сумма") as HTMLInputElement).value).toBe("280000");
    fireEvent.click(await within(form).findByRole("button", { name: /Invesco NASDAQ 100/ }));
    expect((within(form).getByLabelText("Валюта") as HTMLInputElement).value).toBe("RUB");
    fireEvent.click(within(form).getByRole("button", { name: "Добавить в план" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => new URL((r as Request).url).pathname.endsWith("/what-if"))).toBe(true));
    const post = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => new URL(r.url).pathname.endsWith("/what-if"))!;
    expect(await post.json()).toEqual({
      operations: [{ kind: "buy", account_id: "a-broker", to_account_id: null, instrument_id: "i-qqqm", currency: "RUB", amount_minor: 280_000_00 }],
    });
    expect(within(screen.getByTestId("plan-pending")).getAllByRole("listitem")).toHaveLength(1);
  });

  it("opens a saved plan valued anew and keeps it again under its name", async () => {
    show("owner", [october]);
    const row = await screen.findByTestId("plan-saved-row");
    expect(norm(row.textContent)).toMatch(/Октябрь · сохранён 13\.10\.2026, операций: 1/);
    fireEvent.click(within(row).getByRole("button", { name: "открыть" }));
    expect(norm((await screen.findByTestId("plan-opened")).textContent)).toBe("План «Октябрь» сохранён 13.10.2026 — пересчитан по сегодняшним ценам.");
    await waitFor(() => expect(norm(screen.getByTestId("plan-ops").textContent)).toMatch(/Freedom: купить Invesco NASDAQ 100 на 1 000,00 \$/));
    expect(await screen.findByTestId("plan-result")).toBeTruthy();

    const name = screen.getByLabelText("Название плана") as HTMLInputElement;
    expect(name.value).toBe("Октябрь");
    fireEvent.click(screen.getByRole("button", { name: "Сохранить план" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "POST" && new URL((r as Request).url).pathname.endsWith("/allocation/plans"))).toBe(true));
    const saved = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "POST" && new URL(r.url).pathname.endsWith("/allocation/plans"))!;
    expect(await saved.json()).toEqual({ name: "Октябрь", operations: october.operations });
  });

  it("lets a viewer open a saved plan but neither keep nor forget one", async () => {
    show("viewer", [october]);
    const row = await screen.findByTestId("plan-saved-row");
    expect(within(row).queryByRole("button", { name: "удалить" })).toBeNull();
    fireEvent.click(within(row).getByRole("button", { name: "открыть" }));
    await screen.findByTestId("plan-ops");
    expect(screen.queryByTestId("plan-save")).toBeNull();
  });
});
