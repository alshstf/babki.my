import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
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
import { CategoriesPage } from "./categories";
import { treeOf, type Category } from "@/api/categories";
import type { SessionInfo } from "@/api/session";

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

type Route = { path: string; method?: string; status?: number; body?: unknown };

// A fresh Response per call: a body can be read only once.
function serve(routes: Route[]) {
  fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : String(input);
    const method = (input instanceof Request ? input.method : init?.method) ?? "GET";
    const path = new URL(url, "http://localhost").pathname;
    const match = routes.find(
      (r) => path.startsWith(r.path) && (r.method ?? "GET").toUpperCase() === method.toUpperCase(),
    );
    return Promise.resolve(
      new Response(JSON.stringify(match?.body ?? null), {
        status: match ? (match.status ?? 200) : 404,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

// The requests sent with the given method: their paths and bodies.
async function sent(method: string): Promise<{ path: string; body: unknown }[]> {
  const calls = fetchMock.mock.calls.filter(([input, init]) => {
    const m = (input instanceof Request ? input.method : (init as RequestInit | undefined)?.method) ?? "GET";
    return m.toUpperCase() === method;
  });
  return Promise.all(
    calls.map(async ([input]) => {
      const req = input as Request;
      const text = await req.clone().text();
      return { path: new URL(req.url).pathname, body: text ? JSON.parse(text) : null };
    }),
  );
}

let seq = 0;
function cat(name: string, overrides: Partial<Category> = {}): Category {
  seq++;
  return {
    id: `c${seq}`,
    kind: "expense",
    name,
    parent_id: null,
    archived: false,
    position: seq,
    ...overrides,
  };
}

function session(role: SessionInfo["role"]): SessionInfo {
  return {
    user: { id: "u1", username: "alex", display_name: "Alex" },
    role,
    space_id: "s1",
    space_name: "Family",
    base_currency: "RUB",
    full_valuation: "nav_and_foreign",
    tax_residency: "RU",
    cost_basis_rules: { country: "RU", method: "fifo", perimeter: "account", supported: true, notices: [] },
  };
}

function renderPage(role: SessionInfo["role"] = "owner") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session(role));
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const page = createRoute({ getParentRoute: () => rootRoute, path: "/settings/categories", component: CategoriesPage });
  const settings = createRoute({ getParentRoute: () => rootRoute, path: "/settings", component: () => null });
  const router = createRouter({
    routeTree: rootRoute.addChildren([page, settings]),
    history: createMemoryHistory({ initialEntries: ["/settings/categories"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

// Radix's menu opens on pointerdown, which jsdom lacks; Enter opens it too.
async function openActions(name: string) {
  fireEvent.keyDown(await screen.findByRole("button", { name: `Действия с категорией «${name}»` }), { key: "Enter" });
}

const home = cat("Дом");
const rent = cat("Аренда", { parent_id: home.id });
const food = cat("Продукты");
const oldHobby = cat("Рыбалка", { archived: true });
const salary = cat("Зарплата", { kind: "income" });

beforeEach(() => {
  fetchMock.mockReset();
});

describe("treeOf", () => {
  it("puts each kind's categories in order, children under their parent", () => {
    const later = cat("Связь", { position: 0 });
    const tree = treeOf([food, rent, home, salary, later], "expense");
    expect(tree.map((n) => n.name)).toEqual(["Связь", "Дом", "Продукты"]);
    expect(tree[1].children.map((c) => c.name)).toEqual(["Аренда"]);
    expect(treeOf([food, salary], "income").map((n) => n.name)).toEqual(["Зарплата"]);
  });

  it("leaves out a child whose parent is not there", () => {
    expect(treeOf([rent], "expense")).toEqual([]);
  });
});

describe("CategoriesPage", () => {
  it("shows both kinds as trees and hides the archive until asked", async () => {
    serve([{ path: "/api/v1/categories", body: [home, rent, food, oldHobby, salary] }]);
    renderPage();

    const expense = await screen.findByTestId("categories-expense");
    expect(within(expense).getByText("Аренда")).toBeInTheDocument();
    expect(within(screen.getByTestId("categories-income")).getByText("Зарплата")).toBeInTheDocument();
    expect(within(expense).queryByText("Рыбалка")).toBeNull();

    fireEvent.click(screen.getByTestId("categories-show-archived"));
    expect(within(expense).getByText("Рыбалка")).toBeInTheDocument();
    expect(within(expense).getByText("в архиве")).toBeInTheDocument();
  });

  it("adds a subcategory under the chosen parent", async () => {
    serve([
      { path: "/api/v1/categories", body: [home, rent, food, salary] },
      { path: "/api/v1/categories", method: "POST", status: 201, body: cat("Коммуналка", { parent_id: home.id }) },
    ]);
    renderPage();

    await openActions("Дом");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Добавить подкатегорию" }));
    const dialog = await screen.findByTestId("category-dialog");
    fireEvent.change(within(dialog).getByLabelText("Название"), { target: { value: "  Коммуналка " } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));

    await waitFor(async () => expect(await sent("POST")).toHaveLength(1));
    expect((await sent("POST"))[0].body).toEqual({ kind: "expense", name: "Коммуналка", parent_id: home.id });
    await waitFor(() => expect(screen.queryByTestId("category-dialog")).toBeNull());
  });

  it("says a name is taken at that place before sending it", async () => {
    serve([{ path: "/api/v1/categories", body: [home, rent, food, salary] }]);
    renderPage();

    fireEvent.click(await screen.findByTestId("categories-add-expense"));
    const dialog = await screen.findByTestId("category-dialog");
    fireEvent.change(within(dialog).getByLabelText("Название"), { target: { value: "продукты" } });

    expect(within(dialog).getByText("Такая категория здесь уже есть.")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Сохранить" })).toBeDisabled();
  });

  it("keeps a category with children on the top level", async () => {
    serve([{ path: "/api/v1/categories", body: [home, rent, food, salary] }]);
    renderPage();

    await openActions("Дом");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Изменить" }));
    const dialog = await screen.findByTestId("category-dialog");
    expect(within(dialog).queryByRole("combobox", { name: "Где" })).toBeNull();
    expect(within(dialog).getByText(/остаётся на верхнем уровне/)).toBeInTheDocument();
  });

  it("archives from the menu and explains a refused removal", async () => {
    serve([
      { path: "/api/v1/categories", body: [home, rent, food, salary] },
      { path: "/api/v1/categories/", method: "PATCH", body: { ...food, archived: true } },
      { path: "/api/v1/categories/", method: "DELETE", status: 422, body: { error: "category is in use" } },
    ]);
    renderPage();

    await openActions("Продукты");
    fireEvent.click(await screen.findByRole("menuitem", { name: "В архив" }));
    await waitFor(async () => expect(await sent("PATCH")).toHaveLength(1));
    expect((await sent("PATCH"))[0]).toEqual({ path: `/api/v1/categories/${food.id}`, body: { archived: true } });

    await openActions("Продукты");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Удалить" }));
    expect(await screen.findByTestId("categories-error")).toHaveTextContent("Отправьте её в архив");
  });

  it("lets a viewer read but not change", async () => {
    serve([{ path: "/api/v1/categories", body: [home, rent, food, salary] }]);
    renderPage("viewer");

    expect(await screen.findByText("Аренда")).toBeInTheDocument();
    expect(screen.queryByTestId("categories-add-expense")).toBeNull();
    expect(screen.queryByRole("button", { name: /Действия с категорией/ })).toBeNull();
  });
});

describe("CategoryRules", () => {
  const rule = (id: string, pattern: string, categoryId: string, position: number) => ({
    id, category_id: categoryId, field: "counterparty", pattern, position,
  });

  it("reads each rule as a sentence and moves one down", async () => {
    serve([
      { path: "/api/v1/categories", body: [home, rent, food, salary] },
      { path: "/api/v1/category-rules", body: [rule("r1", "Пятёрочка", food.id, 0), rule("r2", "ИП Смирнова", rent.id, 1)] },
      { path: "/api/v1/category-rules/order", method: "PUT", status: 204 },
    ]);
    renderPage();

    const rows = await screen.findAllByTestId("category-rule");
    expect(rows[0]).toHaveTextContent("Контрагент содержит «Пятёрочка» → Продукты");
    expect(rows[1]).toHaveTextContent("Дом › Аренда");
    fireEvent.click(within(rows[0]).getByRole("button", { name: "Ниже" }));
    await waitFor(async () => expect(await sent("PUT")).toHaveLength(1));
    expect((await sent("PUT"))[0].body).toEqual({ ids: ["r2", "r1"] });
  });

  it("adds a rule", async () => {
    serve([
      { path: "/api/v1/categories", body: [home, rent, food, salary] },
      { path: "/api/v1/category-rules", body: [] },
      { path: "/api/v1/category-rules", method: "POST", status: 201, body: rule("r3", "Ромашка", salary.id, 0) },
    ]);
    renderPage();

    fireEvent.click(await screen.findByTestId("category-rules-add"));
    const dialog = await screen.findByTestId("category-rule-dialog");
    fireEvent.change(within(dialog).getByLabelText("Текст"), { target: { value: " Ромашка " } });
    fireEvent.keyDown(within(dialog).getByRole("combobox", { name: "Категория" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "Зарплата" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));

    await waitFor(async () => expect(await sent("POST")).toHaveLength(1));
    expect((await sent("POST"))[0].body).toEqual({ field: "counterparty", pattern: "Ромашка", category_id: salary.id });
  });
});
