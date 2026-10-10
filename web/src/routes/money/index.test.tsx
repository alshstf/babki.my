import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import "@/i18n";
import { MoneyPage } from "./index";
import { periodDays } from "@/lib/periods";
import type { CashflowFlow, CashflowLine, CashflowReport } from "@/api/cashflow";
import type { Category } from "@/api/categories";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// jsdom lacks scrollIntoView, which Radix Select calls on open.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

const CATEGORIES: Category[] = [
  { id: "c-tr", kind: "expense", name: "Транспорт", parent_id: null, archived: false, position: 1 },
  { id: "c-taxi", kind: "expense", name: "Такси", parent_id: "c-tr", archived: false, position: 1 },
  { id: "c-salary", kind: "income", name: "Зарплата", parent_id: null, archived: false, position: 1 },
];

const flow = (byMonth: number[]): CashflowFlow => ({ total_minor: byMonth.reduce((a, b) => a + b, 0), by_month: byMonth });
const line = (id: string | null, byMonth: number[], extra: Partial<CashflowLine> = {}): CashflowLine => ({
  category_id: id,
  group: null,
  ...flow(byMonth),
  direct: flow(byMonth),
  children: [],
  ...extra,
});

function report(overrides: Partial<CashflowReport> = {}): CashflowReport {
  const zero = flow([0, 0]);
  return {
    base_currency: "RUB",
    from: "2026-08-01",
    to: "2026-09-30",
    months: ["2026-08", "2026-09"],
    income: { ...flow([180_000_00, 180_000_00]), lines: [line("c-salary", [180_000_00, 180_000_00])] },
    expense: {
      ...flow([640_00, 1_099_00]),
      lines: [
        line("c-tr", [640_00, 1_000_00], { direct: flow([0, 1_000_00]), children: [line("c-taxi", [640_00, 0])] }),
        line(null, [0, 99_00], { group: "fee" }),
      ],
    },
    unfiled_in: zero,
    unfiled_out: flow([0, 15_000_00]),
    investments: { deposited: flow([50_000_00, 0]), withdrawn: zero, payouts: zero, interest: zero, costs: zero },
    missing_rates: [],
    left_out: 0,
    ...overrides,
  };
}

let asked: URL[] = [];
let answer: CashflowReport = report();

beforeEach(() => {
  asked = [];
  answer = report();
  fetchMock.mockImplementation(async (input: Request) => {
    const url = new URL(input.url);
    const json = (body: unknown) =>
      new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    if (url.pathname.endsWith("/cashflow")) {
      asked.push(url);
      return json(answer);
    }
    if (url.pathname.endsWith("/categories")) return json(CATEGORIES);
    if (url.pathname.endsWith("/members")) {
      return json([{ id: "u-1", username: "alex", display_name: "Александр", role: "owner" }]);
    }
    return new Response("null", { status: 404 });
  });
});

afterEach(() => cleanup());

function renderPage() {
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const page = createRoute({ getParentRoute: () => rootRoute, path: "/money", component: MoneyPage });
  const accounts = createRoute({ getParentRoute: () => rootRoute, path: "/accounts", component: () => null });
  const router = createRouter({
    routeTree: rootRoute.addChildren([page, accounts]),
    history: createMemoryHistory({ initialEntries: ["/money"] }),
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("periodDays", () => {
  it("works out each period from today", () => {
    const today = new Date(2026, 9, 10); // 10 October 2026
    expect(periodDays("thisMonth", today)).toEqual({ from: "2026-10-01", to: "2026-10-10" });
    expect(periodDays("lastMonth", today)).toEqual({ from: "2026-09-01", to: "2026-09-30" });
    expect(periodDays("last3", today)).toEqual({ from: "2026-08-01", to: "2026-10-10" });
    expect(periodDays("lastYear", today)).toEqual({ from: "2025-01-01", to: "2025-12-31" });
    expect(periodDays("thisMonth", new Date(2026, 0, 31)).from).toBe("2026-01-01");
    expect(periodDays("last6", new Date(2026, 1, 15)).from).toBe("2025-09-01");
  });
});

describe("MoneyPage", () => {
  it("shows the totals, the categories by month and what waits for filing", async () => {
    renderPage();
    expect(await screen.findByTestId("money-total-income")).toHaveTextContent(/360\s000/);
    expect(screen.getByTestId("money-total-expense")).toHaveTextContent(/1\s739/);
    expect(screen.getByTestId("money-savings-rate")).toHaveTextContent("100 %");

    const spending = screen.getByTestId("money-expense");
    const rows = within(spending).getAllByTestId("money-line").map((r) => r.firstChild?.textContent);
    expect(rows).toEqual(["Транспорт", "без уточнения", "Такси", "Комиссии банка", "Итого"]);
    expect(within(spending).getByRole("columnheader", { name: "Авг" })).toBeTruthy();

    expect(screen.getByTestId("money-unfiled")).toHaveTextContent(/15\s000/);
    expect(screen.getByTestId("money-investments")).toHaveTextContent(/50\s000/);
  });

  it("asks again for another period and for one member's accounts", async () => {
    renderPage();
    await screen.findByTestId("money-expense");
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Чьи счета" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "Александр" }));
    await waitFor(() => expect(asked.some((u) => u.searchParams.get("member") === "u-1")).toBe(true));

    fireEvent.keyDown(screen.getByRole("combobox", { name: "Период" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "Прошлый год" }));
    await waitFor(() => expect(asked.some((u) => u.searchParams.get("to")?.endsWith("-12-31"))).toBe(true));
  });

  it("says which rows were left out for want of a rate", async () => {
    answer = report({ missing_rates: ["KZT"], left_out: 3 });
    renderPage();
    expect(await screen.findByTestId("money-missing-rates")).toHaveTextContent("3 (валюты: KZT)");
  });
});
