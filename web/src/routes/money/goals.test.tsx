import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { GoalsCard } from "./goals";
import type { Goal, Goals } from "@/api/goals";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Radix's select scrolls the picked option into view; jsdom has no layout.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

const account = (id: string, name: string, type: string) => ({
  id, name, type, currency: "RUB", institution: "", status: "active", created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false, trades_abroad: false, kept_by_operations: false, foreign_broker: false, tax_owner_user_id: null, counted_by: "balance",
});
const accounts = [
  account("a-card", "Карта Т-Банка", "checking"),
  account("a-save", "Накопительный", "savings"),
  account("a-broker", "Брокерский", "brokerage"),
  account("a-credit", "Кредитка", "credit_card"),
];

const base: Omit<Goal, "id" | "name"> = {
  target_minor: null, target_on: null, monthly_minor: null, yield_percent: "0.00", growth_percent: "0.000", funds: [], saved_minor: 0, percent: 0,
  status: "none", projected_minor: null, short_minor: null, needed_minor: null, reach_on: null, months: 0, overdrawn: [],
};

// The note's family (Р-44): the cushion on the whole savings account, the
// holiday on 70 000 out of the card and falling short, the first payment out
// of the brokerage account ahead at 10 %.
const family: Goals = {
  base_currency: "RUB",
  monthly_minor: 65_000_00,
  broker_monthly_minor: 30_000_00,
  goals: [
    {
      ...base, id: "g-cushion", name: "Подушка", target_minor: 570_000_00, monthly_minor: 15_000_00, yield_percent: "14.00", growth_percent: "16.000",
      funds: [{ account_id: "a-save", amount_minor: null, value_minor: 450_000_00, rate_percent: "16.000", rate_by_hand: true }], saved_minor: 450_000_00, percent: 78,
      reach_on: "2027-05-01", months: 6,
    },
    {
      ...base, id: "g-holiday", name: "Отпуск", target_minor: 250_000_00, target_on: "2027-07-01", monthly_minor: 20_000_00,
      funds: [{ account_id: "a-card", amount_minor: 70_000_00, value_minor: 70_000_00, rate_percent: null, rate_by_hand: false }], saved_minor: 70_000_00, percent: 28,
      status: "at_risk", projected_minor: 230_000_00, short_minor: 20_000_00, needed_minor: 22_500_00, reach_on: "2027-08-01", months: 8,
    },
    {
      ...base, id: "g-flat", name: "Первый взнос", target_minor: 2_000_000_00, target_on: "2029-09-01", monthly_minor: 30_000_00, yield_percent: "10.00", growth_percent: "10.000",
      funds: [{ account_id: "a-broker", amount_minor: 800_000_00, value_minor: 800_000_00, rate_percent: null, rate_by_hand: false }], saved_minor: 800_000_00, percent: 40,
      status: "ahead", projected_minor: 2_234_000_00, needed_minor: 25_000_00, months: 34,
    },
  ],
};

function answer(role: string, goals: Goals) {
  fetchMock.mockImplementation((input: Request) => {
    const path = new URL(input.url).pathname;
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
    if (path.includes("/goals")) return json(goals);
    if (path.endsWith("/accounts")) return json(accounts);
    if (path.endsWith("/auth/me")) return json({ user: { id: "u", username: "a", display_name: "A" }, role, space: { id: "s", name: "S", base_currency: "RUB" } });
    return json([]);
  });
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <GoalsCard />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");
const sent = async (method: string) => {
  await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === method)).toBe(true));
  return fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === method)!;
};

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("GoalsCard", () => {
  it("tells each goal where it stands and what would do", async () => {
    answer("owner", family);
    show();
    const goals = await screen.findAllByTestId("goal");
    expect(goals).toHaveLength(3);
    const [cushion, holiday, flat] = goals.map((g) => norm(g.textContent));
    expect(cushion).toContain("450 000 ₽ из 570 000 ₽ · 78 %");
    expect(cushion).toContain("При взносе 15 000 ₽ в месяц цель будет собрана к 01.05.2027.");
    await waitFor(() => expect(norm(goals[0].textContent)).toContain("Накопительный — весь счёт, 450 000 ₽ · 16 % годовых, вручную"));
    // The savings account's own rate, not the goal's 14 (decision Р-51 А).
    expect(cushion).toContain("Рост в расчёте — 16 % в год, по ставкам счетов");
    expect(cushion).not.toContain("14 %");
    expect(norm(goals[2].textContent)).toContain("Доходность в расчёте — 10 % в год");
    expect(holiday).toContain("к 01.07.2027");
    expect(within(goals[1]).getByTestId("goal-status").textContent).toBe("не успеваете");
    expect(holiday).toContain("Взносов до срока: 8 по 20 000 ₽ — будет 230 000 ₽.");
    expect(holiday).toContain("Не хватит 20 000 ₽. Чтобы успеть — 22 500 ₽ в месяц или срок до 01.08.2027.");
    expect(norm(goals[1].textContent)).toContain("Карта Т-Банка — 70 000 ₽ из остатка");
    expect(within(goals[2]).getByTestId("goal-status").textContent).toBe("впереди");
    expect(flat).not.toContain("Не хватит");
  });

  it("changes a goal and adds one on an account, a debt not offered", async () => {
    answer("owner", family);
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Отпуск" }));
    const dialog = await screen.findByTestId("goal-dialog");
    fireEvent.change(within(dialog).getByLabelText("Взнос в месяц, RUB"), { target: { value: "22500" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    const put = await sent("PUT");
    expect(new URL(put.url).pathname).toBe("/api/v1/goals/g-holiday");
    expect(await put.json()).toEqual({
      name: "Отпуск", target_minor: 250_000_00, target_on: "2027-07-01", monthly_minor: 22_500_00, yield_percent: "0",
      funds: [{ account_id: "a-card", amount_minor: 70_000_00 }],
    });
    await waitFor(() => expect(screen.queryByTestId("goal-dialog")).toBeNull());

    fireEvent.click(screen.getByRole("button", { name: "Добавить цель" }));
    const add = await screen.findByTestId("goal-dialog");
    fireEvent.change(within(add).getByLabelText("Название"), { target: { value: "Ремонт" } });
    fireEvent.click(within(add).getByRole("button", { name: "+ Счёт" }));
    fireEvent.click(within(add).getByRole("combobox", { name: "Счёт" }));
    const options = await screen.findAllByRole("option");
    expect(options.map((o) => o.textContent)).not.toContain("Кредитка");
    fireEvent.click(options.find((o) => o.textContent === "Брокерский")!);
    fireEvent.click(within(add).getByRole("button", { name: "Сохранить" }));
    const post = await sent("POST");
    expect(await post.json()).toEqual({
      name: "Ремонт", target_minor: null, target_on: null, monthly_minor: null, yield_percent: "0",
      funds: [{ account_id: "a-broker", amount_minor: null }],
    });
  });

  it("warns of an account with more set aside than it holds; a viewer only reads", async () => {
    const over = structuredClone(family);
    over.goals[1].overdrawn = ["a-card"];
    over.goals[1].funds[0].value_minor = 50_000_00;
    answer("viewer", over);
    show();
    await waitFor(() => expect(norm(screen.getByTestId("goals-overdrawn").textContent)).toContain("«Карта Т-Банка»"));
    expect(norm(screen.getAllByTestId("goal")[1].textContent)).toContain("70 000 ₽ из остатка, но на счёте сейчас 50 000 ₽");
    expect(screen.queryByRole("button", { name: "Добавить цель" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Отпуск" })).toBeNull();
  });

  it("shows nothing to a viewer without goals and the empty card to an editor", async () => {
    answer("viewer", { base_currency: "RUB", goals: [], monthly_minor: 0, broker_monthly_minor: 0 });
    show();
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(screen.queryByTestId("money-goals")).toBeNull();
    cleanup();
    answer("editor", { base_currency: "RUB", goals: [], monthly_minor: 0, broker_monthly_minor: 0 });
    show();
    expect(await screen.findByTestId("goals-empty")).toBeTruthy();
  });
});
