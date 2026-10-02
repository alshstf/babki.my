import type { ReactElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
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
import { TableImport } from "./import-page";

const sent = vi.hoisted(() => [] as { method: string; path: string; body: unknown }[]);
const state = vi.hoisted(() => ({ imports: [] as unknown[], missing: false }));

const preview = (hasHeader: boolean) => ({
  mapping: {
    has_header: hasHeader,
    columns: { date: 0, type: 1, amount: 2 },
    types: { пополнение: "deposit" },
  },
  header: hasHeader ? ["Дата", "Тип", "Сумма"] : [],
  rows: [
    {
      line: 2,
      cells: ["01.07.2026", "Пополнение", "500"],
      verdict: "new",
      reason: null,
      operation: {
        type: "deposit",
        occurred_on: "2026-07-01",
        instrument_id: null,
        quantity: null,
        price: null,
        amount_minor: 50_000,
        currency: "RUB",
        fee_minor: 0,
        note: "",
      },
    },
    { line: 3, cells: ["02.07.2026", "Перевод", "100"], verdict: "unparsed", reason: { code: "type_not_mapped", field: "type", value: "Перевод" }, operation: null },
  ],
});

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});
const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
fetchMock.mockImplementation(async (input: Request) => {
  const path = new URL(input.url).pathname;
  const body = input.method === "GET" || input.method === "DELETE" ? null : await input.clone().json();
  if (input.method !== "GET") sent.push({ method: input.method, path, body });
  if (path === "/api/v1/accounts") return json([]);
  if (path.endsWith("/imports/preview")) {
    const mapping = (body as { mapping?: { has_header: boolean } }).mapping;
    const answer = preview(mapping ? mapping.has_header : true);
    if (state.missing) {
      answer.rows.push({
        line: 4,
        cells: ["03.07.2026", "Покупка", "GAZP"],
        verdict: "unparsed",
        reason: { code: "paper_not_found", field: "instrument", value: "GAZP" },
        operation: null,
      } as never);
    }
    return json(answer);
  }
  if (path === "/api/v1/imports/papers") {
    state.missing = false;
    return json({
      added: [{ code: "GAZP", instrument_id: "i-gazp", name: "ГАЗПРОМ ао", ticker: "GAZP" }],
      known: [],
      not_found: [],
    });
  }
  if (path.endsWith("/imports") && input.method === "POST") {
    const imported = {
      id: "imp-1",
      account_id: "acc-1",
      file_name: "alfa.csv",
      mapping: preview(true).mapping,
      rows_written: 1,
      rows_duplicate: 0,
      rows_unparsed: 1,
      rows_refused: 0,
      created_at: "2026-10-03T01:00:00Z",
      rolled_back_at: null,
      operations_left: 1,
    };
    state.imports = [imported];
    return json({ import: imported, rows: preview(true).rows });
  }
  if (path.endsWith("/imports")) return json(state.imports);
  if (path.startsWith("/api/v1/imports/")) return json({ ...(state.imports[0] as object), rolled_back_at: "2026-10-03T02:00:00Z" });
  if (path === "/api/v1/instruments") return json({ instruments: [], has_more: false });
  return new Response("null", { status: 404 });
});

afterEach(() => {
  cleanup();
  sent.length = 0;
  state.imports = [];
  state.missing = false;
});

function wrap(ui: ReactElement) {
  const rootRoute = createRootRoute();
  const page = createRoute({ getParentRoute: () => rootRoute, path: "/", component: () => ui });
  const detail = createRoute({ getParentRoute: () => rootRoute, path: "/accounts/$accountId", component: () => null });
  const router = createRouter({
    routeTree: rootRoute.addChildren([page, detail]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("importing a table", () => {
  it("previews the file, lets the mapping be changed, imports, and offers the import back", async () => {
    wrap(<TableImport accountId="acc-1" />);
    const file = new File(["Дата;Тип;Сумма\n01.07.2026;Пополнение;500\n02.07.2026;Перевод;100\n"], "alfa.csv");
    fireEvent.change(await screen.findByLabelText("Файл"), { target: { files: [file] } });

    await screen.findByText("будет записано: 1");
    expect(sent[0]).toMatchObject({ method: "POST", path: "/api/v1/accounts/acc-1/imports/preview" });
    expect((sent[0].body as { mapping?: unknown }).mapping).toBeUndefined();
    expect(screen.getByText("не разобрано: 1")).toBeTruthy();
    expect(screen.getByTestId("import-row-3").textContent).toContain("не разобрана");
    expect(screen.getByTestId("import-row-3").textContent).toContain(
      "«Перевод» не сопоставлено ни с одной операцией",
    );

    // The header box unticked: asked again, with the mapping as now set.
    fireEvent.click(screen.getByLabelText("Первая строка — заголовок"));
    await waitFor(() => expect(sent).toHaveLength(2));
    expect(sent[1].body).toMatchObject({ mapping: { has_header: false, columns: { date: 0 } } });

    await waitFor(() => expect(screen.getByRole("button", { name: "Импортировать (1)" })).not.toBeDisabled());
    fireEvent.click(screen.getByRole("button", { name: "Импортировать (1)" }));
    expect((await screen.findByTestId("import-done")).textContent).toContain("Записано операций: 1");
    expect(screen.getByTestId("import-count-new").textContent).toBe("записано: 1");
    expect(screen.getByTestId("import-row-2").textContent).toContain("записана");
    expect(screen.getByTestId("import-row-2").textContent).not.toContain("будет записана");
    const imported = sent.find((s) => s.path === "/api/v1/accounts/acc-1/imports");
    expect(imported?.body).toMatchObject({ file_name: "alfa.csv", mapping: { has_header: false } });

    fireEvent.click(await screen.findByRole("button", { name: "Откатить" }));
    const confirm = await screen.findByRole("dialog");
    expect(sent.some((s) => s.method === "DELETE")).toBe(false);
    fireEvent.click(within(confirm).getByRole("button", { name: "Откатить" }));
    await waitFor(() => expect(sent.some((s) => s.method === "DELETE" && s.path === "/api/v1/imports/imp-1")).toBe(true));
  });

  it("files the papers the catalog lacks from the exchange and reads the rows again", async () => {
    state.missing = true;
    wrap(<TableImport accountId="acc-1" />);
    const file = new File(["Дата;Тип;Сумма\n"], "a.csv");
    fireEvent.change(await screen.findByLabelText("Файл"), { target: { files: [file] } });

    expect((await screen.findByTestId("import-missing-papers")).textContent).toContain(
      "Этих бумаг нет в каталоге: GAZP",
    );
    fireEvent.click(screen.getByRole("button", { name: "Найти на Мосбирже и добавить (1)" }));
    expect((await screen.findByTestId("import-papers-added")).textContent).toContain("ГАЗПРОМ ао (GAZP)");
    expect(sent.find((s) => s.path === "/api/v1/imports/papers")?.body).toEqual({ codes: ["GAZP"] });
    await waitFor(() => expect(sent.filter((s) => s.path.endsWith("/imports/preview"))).toHaveLength(2));
  });

  it("remembers what the type column's words meant in the account's last import", async () => {
    state.imports = [
      {
        id: "imp-0",
        account_id: "acc-1",
        file_name: "old.csv",
        mapping: { has_header: true, columns: { date: 0, type: 1, amount: 2 }, types: { перевод: "withdrawal", пополнение: "interest" } },
        rows_written: 1,
        rows_duplicate: 0,
        rows_unparsed: 0,
        rows_refused: 0,
        created_at: "2026-09-01T10:00:00Z",
        rolled_back_at: null,
        operations_left: 1,
      },
    ];
    wrap(<TableImport accountId="acc-1" />);
    await screen.findByText("Импорты этого счёта");
    const file = new File(["Дата;Тип;Сумма\n"], "new.csv");
    fireEvent.change(screen.getByLabelText("Файл"), { target: { files: [file] } });

    await waitFor(() => expect(sent.filter((s) => s.path.endsWith("/imports/preview"))).toHaveLength(2));
    const second = sent.filter((s) => s.path.endsWith("/imports/preview"))[1];
    expect(second.body).toMatchObject({
      mapping: { types: { пополнение: "interest", перевод: "withdrawal" } },
    });
  });
});
