import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { QuickAdd } from "./quick-add";
import type { AccountWithBalance } from "@/api/accounts";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const base = {
  institution: "",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  trades_abroad: false,
  kept_by_operations: true,
  counted_by: "journal",
  balance: { as_of: "2026-07-20", amount_minor: 0 },
} as const;

const broker: AccountWithBalance = { ...base, id: "br", name: "Брокерский", type: "brokerage", currency: "RUB" };
const current: AccountWithBalance = { ...base, id: "cur", name: "Текущий Сбер", type: "checking", currency: "RUB" };
const card: AccountWithBalance = { ...base, id: "card", name: "Кредитка Альфа", type: "credit_card", currency: "RUB" };
const dollars: AccountWithBalance = { ...base, id: "usd", name: "Доллары", type: "cash", currency: "USD" };

function answer(accounts: AccountWithBalance[]) {
  fetchMock.mockImplementation((input: Request) => {
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
    if (input.url.endsWith("/accounts")) return json(accounts);
    if (input.method === "POST") {
      return input.clone().json().then((body: Record<string, unknown>) =>
        json({ ...body, id: "op-1", source: "manual", created_at: "2026-10-10T00:00:00Z" }, 201),
      );
    }
    return json([]);
  });
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <QuickAdd />
    </QueryClientProvider>,
  );
}

beforeEach(() => localStorage.clear());
afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("QuickAdd", () => {
  it("is not there when the family has no everyday account", async () => {
    answer([broker]);
    const { container } = show();
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(container.querySelector("button")).toBeNull();
  });

  it("opens a spending on the card first, without the broker's account", async () => {
    answer([broker, current, card]);
    show();
    fireEvent.click((await screen.findAllByRole("button", { name: "Операция" }))[0]);
    const account = await screen.findByRole("combobox", { name: "Счёт" });
    expect(account.textContent).toContain("Кредитка Альфа");
    // Straight to the amount: on a phone the keyboard opens at once.
    await waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText(/Сумма, RUB/)));
    expect(screen.getByRole("combobox", { name: "Тип операции" }).textContent).toBe("вывод");
  });

  it("goes back to the account the last entry went to, and remembers the next", async () => {
    localStorage.setItem("babki.quickAdd.account", "usd");
    answer([current, card, dollars]);
    show();
    fireEvent.click((await screen.findAllByRole("button", { name: "Операция" }))[0]);
    expect((await screen.findByRole("combobox", { name: "Счёт" })).textContent).toContain("Доллары");
    // The amount field speaks the chosen account's currency.
    fireEvent.change(screen.getByLabelText(/Сумма, USD/), { target: { value: "12" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "POST")).toBe(true));
    const post = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "POST")!;
    expect(await post.clone().json()).toMatchObject({ account_id: "usd", currency: "USD", amount_minor: -1200, type: "withdrawal" });
    await waitFor(() => expect(localStorage.getItem("babki.quickAdd.account")).toBe("usd"));
  });
});
