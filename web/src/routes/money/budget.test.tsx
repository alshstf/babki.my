import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { BudgetCard } from "./budget";
import type { Budget } from "@/api/budget";
import { localToday } from "@/lib/dates";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const categories = [
  { id: "c-food", kind: "expense", name: "Продукты", parent_id: null, archived: false, position: 1 },
  { id: "c-cafe", kind: "expense", name: "Кафе", parent_id: null, archived: false, position: 2 },
  { id: "c-trips", kind: "expense", name: "Путешествия", parent_id: null, archived: false, position: 3 },
  { id: "c-gifts", kind: "expense", name: "Подарки", parent_id: null, archived: false, position: 4 },
];

// Decision Р-25's September: groceries in the limit, the café over it, the
// trip's копилка with 15 000 from the months before.
const september: Budget = {
  month: "2026-09", base_currency: "RUB",
  lines: [
    { category_id: "c-food", limit_minor: 30_000_00, rollover: false, since: "2026-06", carried_minor: 0, spent_minor: 26_000_00, left_minor: 4_000_00 },
    { category_id: "c-cafe", limit_minor: 4_000_00, rollover: false, since: "2026-06", carried_minor: 0, spent_minor: 5_100_00, left_minor: -1_100_00 },
    { category_id: "c-trips", limit_minor: 5_000_00, rollover: true, since: "2026-06", carried_minor: 15_000_00, spent_minor: 18_500_00, left_minor: 1_500_00 },
  ],
  planned_minor: 54_000_00, spent_minor: 49_600_00, left_minor: 4_400_00, unlimited_minor: 52_990_00, missing_rates: [],
};

function answer(role: string, budget: Budget) {
  fetchMock.mockImplementation((input: Request) => {
    const path = new URL(input.url).pathname;
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(body === null ? null : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
    if (path.endsWith("/budget/limits")) return json(null, 204);
    if (path.endsWith("/budget")) return json(budget);
    if (path.endsWith("/categories")) return json(categories);
    if (path.endsWith("/auth/me")) return json({ user: { id: "u", username: "a", display_name: "A" }, role, space: { id: "s", name: "S", base_currency: "RUB" } });
    return json([]);
  });
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <BudgetCard />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");
const puts = async () => {
  await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
  return (fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!).json();
};

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("BudgetCard", () => {
  it("shows the month's limits, the копилка and the spending without a limit", async () => {
    answer("owner", september);
    show();
    const lines = await screen.findAllByTestId("budget-line");
    expect(lines).toHaveLength(3);
    expect(norm(lines[2].textContent)).toContain("Путешествия · копилка: +15 000 ₽");
    expect(norm(lines[1].textContent)).toContain("-1 100 ₽");
    expect(within(lines[1]).getAllByRole("cell")[3].className).toContain("text-red-700");
    expect(norm(screen.getByTestId("budget-sums").textContent)).toContain("54 000 ₽");
    expect(norm(screen.getByTestId("budget-unlimited").textContent)).toContain("52 990 ₽");
  });

  it("changes a line's limit and takes one off from the month shown", async () => {
    answer("owner", september);
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Путешествия" }));
    const dialog = await screen.findByTestId("budget-limit-dialog");
    fireEvent.change(within(dialog).getByLabelText("Лимит в месяц, RUB"), { target: { value: "6000" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    expect(await puts()).toEqual({ category_id: "c-trips", from_month: localToday().slice(0, 7), amount_minor: 6_000_00, rollover: true });

    fetchMock.mockClear();
    fireEvent.click(await screen.findByRole("button", { name: "Кафе" }));
    fireEvent.click(within(await screen.findByTestId("budget-limit-dialog")).getByRole("button", { name: "Убрать лимит" }));
    expect(await puts()).toMatchObject({ category_id: "c-cafe", amount_minor: 0, rollover: false });
  });

  it("sets a new limit with a копилка", async () => {
    answer("editor", september);
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Задать лимит" }));
    const dialog = await screen.findByTestId("budget-limit-dialog");
    Element.prototype.scrollIntoView ??= () => {};
    fireEvent.keyDown(within(dialog).getByRole("combobox"), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "Подарки" }));
    fireEvent.change(within(dialog).getByLabelText("Лимит в месяц, RUB"), { target: { value: "2000" } });
    fireEvent.click(within(dialog).getByLabelText("Копилка"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    expect(await puts()).toEqual({ category_id: "c-gifts", from_month: localToday().slice(0, 7), amount_minor: 2_000_00, rollover: true });
  });

  it("steps to another month", async () => {
    answer("owner", september);
    show();
    await screen.findAllByTestId("budget-line");
    fireEvent.click(screen.getByRole("button", { name: "Предыдущий месяц" }));
    const this_ = localToday().slice(0, 7);
    const d = new Date(Date.UTC(Number(this_.slice(0, 4)), Number(this_.slice(5, 7)) - 2, 1)).toISOString().slice(0, 7);
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([r]) => new URL((r as Request).url).searchParams.get("month") === d)).toBe(true),
    );
  });

  it("is nothing for a viewer while no limit is set", async () => {
    answer("viewer", { ...september, lines: [] });
    show();
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => new URL((r as Request).url).pathname.endsWith("/budget"))).toBe(true));
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByTestId("money-budget")).toBeNull();
  });
});
