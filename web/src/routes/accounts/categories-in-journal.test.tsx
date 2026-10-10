import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import "@/i18n";
import { CashDialog } from "./cash-dialog";
import { OperationsTable } from "./operations-table";
import type { AccountWithBalance } from "@/api/accounts";
import { rulePattern, type Category } from "@/api/categories";
import type { Operation } from "@/api/operations";

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

const food: Category = { id: "c-food", kind: "expense", name: "Продукты", parent_id: null, archived: false, position: 1 };
const transport: Category = { id: "c-tr", kind: "expense", name: "Транспорт", parent_id: null, archived: false, position: 2 };
const taxi: Category = { id: "c-taxi", kind: "expense", name: "Такси", parent_id: "c-tr", archived: false, position: 1 };
const salary: Category = { id: "c-salary", kind: "income", name: "Зарплата", parent_id: null, archived: false, position: 1 };
const CATEGORIES = [food, transport, taxi, salary];

const card: AccountWithBalance = {
  id: "acc-1",
  name: "Карта",
  type: "checking",
  currency: "RUB",
  institution: "",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  trades_abroad: false,
  counted_by: "balance",
};

function makeOperation(overrides: Partial<Operation> = {}): Operation {
  return {
    id: "op-1",
    account_id: "acc-1",
    instrument_id: null,
    type: "withdrawal",
    occurred_on: "2026-09-02",
    settled_on: null,
    quantity: null,
    price: null,
    amount_minor: -2_500_00,
    currency: "RUB",
    fee_minor: 0,
    note: "",
    transfer_group_id: null,
    split_ratio: null,
    source: "tinvest",
    created_at: "2026-09-02T00:00:00Z",
    has_undated_lots: false,
    assembled_from_lots: false,
    categorizable: true,
    category_id: null,
    counterparty: "Пятёрочка",
    ...overrides,
  };
}

const RULES = [{ id: "r-1", category_id: "c-food", field: "counterparty", pattern: "пятёрочка", position: 0 }];

type Sent = { method: string; path: string; search: string; body: unknown };
let sent: Sent[] = [];
let journal: Operation[] = [];

beforeEach(() => {
  sent = [];
  journal = [makeOperation()];
  fetchMock.mockImplementation(async (input: Request) => {
    const url = new URL(input.url);
    const text = await input.clone().text();
    sent.push({ method: input.method, path: url.pathname, search: url.search, body: text ? JSON.parse(text) : null });
    const json = (body: unknown, status = 200) =>
      new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
    if (url.pathname.endsWith("/categories")) return json(CATEGORIES);
    if (url.pathname.endsWith("/category-rules") && input.method === "GET") return json(RULES);
    if (url.pathname.endsWith("/category-rules")) return json({ id: "r-new", position: 1, ...JSON.parse(text) }, 201);
    if (url.pathname.endsWith("/file-by-rules")) return json({ filed: 2 });
    if (url.pathname.endsWith("/category")) {
      const body = JSON.parse(text) as { category_id: string | null };
      return json({ ...journal[0], category_id: body.category_id });
    }
    if (url.pathname.endsWith("/operations") && input.method === "GET") return json({ operations: journal, has_more: false });
    if (url.pathname.endsWith("/operations")) return json(makeOperation({ id: "op-new" }), 201);
    return json({ instruments: [], has_more: false });
  });
});

afterEach(() => {
  cleanup();
});

function client() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

function renderTable(canDelete = true) {
  const rootRoute = createRootRoute();
  const page = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: () => <OperationsTable accountId="acc-1" canDelete={canDelete} mode="native" baseCurrency="RUB" />,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([page]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client()}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

// Radix's Select opens on pointerdown, which jsdom lacks; Enter opens it too.
async function pick(combobox: HTMLElement, option: string) {
  fireEvent.keyDown(combobox, { key: "Enter" });
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("a spending entered by hand", () => {
  it("is a withdrawal with the category and the shop it went to", async () => {
    render(
      <QueryClientProvider client={client()}>
        <CashDialog open onOpenChange={() => {}} account={card} preset="expense" />
      </QueryClientProvider>,
    );
    expect(screen.getByRole("heading", { name: "Расход" })).toBeTruthy();
    fireEvent.change(screen.getByLabelText(/Сумма/), { target: { value: "2500" } });
    await waitFor(() => expect(sent.some((s) => s.path.endsWith("/categories"))).toBe(true));
    await pick(screen.getByRole("combobox", { name: "Категория" }), "Такси");
    fireEvent.change(screen.getByLabelText(/Кому/), { target: { value: " Яндекс Go " } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

    await waitFor(() => expect(sent.filter((s) => s.method === "POST")).toHaveLength(1));
    expect(sent.find((s) => s.method === "POST")?.body).toMatchObject({
      type: "withdrawal",
      amount_minor: -250_000,
      category_id: "c-taxi",
      counterparty: "Яндекс Go",
    });
  });

  it("offers only earning categories for an earning, and drops a spending one on the switch", async () => {
    render(
      <QueryClientProvider client={client()}>
        <CashDialog open onOpenChange={() => {}} account={card} preset="expense" />
      </QueryClientProvider>,
    );
    await waitFor(() => expect(sent.some((s) => s.path.endsWith("/categories"))).toBe(true));
    await pick(screen.getByRole("combobox", { name: "Категория" }), "Продукты");
    await pick(screen.getByRole("combobox", { name: "Тип операции" }), "пополнение");

    const category = screen.getByRole("combobox", { name: "Категория" });
    expect(category).toHaveTextContent("Без категории");
    fireEvent.keyDown(category, { key: "Enter" });
    expect(await screen.findByRole("option", { name: "Зарплата" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "Продукты" })).toBeNull();
  });
});

describe("the journal's categories", () => {
  it("files a broker's row from the journal", async () => {
    renderTable();
    const chip = await screen.findByTestId("operation-category");
    expect(chip).toHaveTextContent("Указать категорию");
    expect(screen.getByTestId("operation-counterparty")).toHaveTextContent("Пятёрочка");

    fireEvent.click(chip);
    const list = await screen.findByRole("listbox", { name: "Категории" });
    expect(within(list).queryByRole("option", { name: "Зарплата" })).toBeNull();
    fireEvent.click(within(list).getByRole("option", { name: "Такси" }));

    await waitFor(() => expect(sent.filter((s) => s.method === "PUT")).toHaveLength(1));
    const put = sent.find((s) => s.method === "PUT");
    expect(put?.path).toBe("/api/v1/operations/op-1/category");
    expect(put?.body).toEqual({ category_id: "c-taxi" });
  });

  it("finds a category by a word of it or of its parent", async () => {
    renderTable();
    fireEvent.click(await screen.findByTestId("operation-category"));
    fireEvent.change(await screen.findByRole("textbox", { name: "Найти категорию" }), { target: { value: "транс" } });
    const list = screen.getByRole("listbox", { name: "Категории" });
    expect(within(list).getAllByRole("option").map((o) => o.textContent)).toEqual(["Транспорт", "Транспорт › Такси"]);
  });

  it("names a child with its parent, and only shows it to a reader", async () => {
    journal = [makeOperation({ category_id: "c-taxi" })];
    renderTable(false);
    const label = await screen.findByTestId("operation-category");
    expect(label.tagName).toBe("SPAN");
    expect(label).toHaveTextContent("Транспорт › Такси");
  });

  it("narrows to the rows still waiting for a category", async () => {
    renderTable();
    await screen.findByTestId("operation-category");
    await pick(screen.getByRole("combobox", { name: "Категория" }), "Без категории");
    await waitFor(() =>
      expect(sent.some((s) => s.path.endsWith("/operations") && s.search.includes("category=none"))).toBe(true),
    );
  });
});

describe("rulePattern", () => {
  it("drops the trailing words with digits, keeping the first", () => {
    expect(rulePattern("ПЯТЕРОЧКА 4411")).toBe("ПЯТЕРОЧКА");
    expect(rulePattern("  YANDEX*GO  MOSCOW 12  ")).toBe("YANDEX*GO MOSCOW");
    expect(rulePattern("ООО «Ромашка»")).toBe("ООО «Ромашка»");
    expect(rulePattern("7-Eleven")).toBe("7-Eleven");
  });
});

describe("filing rules", () => {
  it("suggests a category from the counterparty until one is chosen", async () => {
    render(
      <QueryClientProvider client={client()}>
        <CashDialog open onOpenChange={() => {}} account={card} preset="expense" />
      </QueryClientProvider>,
    );
    await waitFor(() => expect(sent.some((s) => s.path.endsWith("/category-rules"))).toBe(true));
    await waitFor(() => expect(sent.some((s) => s.path.endsWith("/categories"))).toBe(true));
    const counterparty = screen.getByLabelText(/Кому/);
    fireEvent.change(counterparty, { target: { value: "ПЯТЕРОЧКА 4411" } });
    expect(screen.getByRole("combobox", { name: "Категория" })).toHaveTextContent("Продукты");

    await pick(screen.getByRole("combobox", { name: "Категория" }), "Такси");
    fireEvent.change(counterparty, { target: { value: "Пятёрочка у дома" } });
    expect(screen.getByRole("combobox", { name: "Категория" })).toHaveTextContent("Такси");
  });

  it("remembers a counterparty from the journal and files the rows like it", async () => {
    renderTable();
    fireEvent.click(await screen.findByTestId("operation-category"));
    fireEvent.click(await screen.findByTestId("category-remember"));
    fireEvent.click(within(screen.getByRole("listbox", { name: "Категории" })).getByRole("option", { name: "Продукты" }));

    await waitFor(() => expect(sent.some((s) => s.path.endsWith("/file-by-rules"))).toBe(true));
    const rule = sent.find((s) => s.method === "POST" && s.path.endsWith("/category-rules"));
    expect(rule?.body).toEqual({ category_id: "c-food", field: "any", pattern: "Пятёрочка" });
    expect(sent.find((s) => s.path.endsWith("/file-by-rules"))?.body).toEqual({ account_id: "acc-1" });
  });

  it("files the waiting rows from the journal's «без категории»", async () => {
    renderTable();
    await screen.findByTestId("operation-category");
    await pick(screen.getByRole("combobox", { name: "Категория" }), "Без категории");
    fireEvent.click(await screen.findByRole("button", { name: "Разнести по правилам" }));
    expect(await screen.findByText("Разнесено строк: 2")).toBeTruthy();
  });
});
