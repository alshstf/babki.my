import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { UnparsedList } from "./unparsed-list";
import type { TinvestUnparsedOperation } from "@/api/connections";

// openapi-fetch captures globalThis.fetch at import time, so the double has to
// be installed before the imports above run.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call — a body is readable once, and the paging test
// makes two requests.
function servePages(pages: { operations: TinvestUnparsedOperation[]; has_more: boolean }[]) {
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = new URL(input instanceof Request ? input.url : String(input), "http://localhost");
    const offset = Number(url.searchParams.get("offset") ?? "0");
    const page = pages[offset === 0 ? 0 : 1] ?? { operations: [], has_more: false };
    return Promise.resolve(
      new Response(JSON.stringify(page), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

function serve(operations: TinvestUnparsedOperation[], hasMore = false) {
  servePages([{ operations, has_more: hasMore }]);
}

function makeOperation(overrides: Partial<TinvestUnparsedOperation> = {}): TinvestUnparsedOperation {
  return {
    id: "u-1",
    link_id: "link-1",
    content_key: "key-u-1",
    occurred_at: "2026-08-04T09:15:00Z",
    op_type: "OPERATION_TYPE_FUTURES_VARIATION_MARGIN",
    payment: "-1234.5",
    currency: "RUB",
    description: "Вариационная маржа",
    reason: "unsupported_type",
    detail: 'broker operation type "OPERATION_TYPE_FUTURES_VARIATION_MARGIN"',
    raw: { id: "broker-op-1", operation_type: "OPERATION_TYPE_FUTURES_VARIATION_MARGIN" },
    ...overrides,
  };
}

function renderList() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <UnparsedList connectionId="conn-1" />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  fetchMock.mockReset();
  serve([]);
});

describe("UnparsedList", () => {
  it("names the real cause in Russian, beside the broker's own words for the operation", async () => {
    serve([makeOperation()]);
    renderList();

    expect(await screen.findByText("Тип операции пока не поддерживается")).toBeInTheDocument();
    // The broker's own type word, kept as it came: this row exists precisely
    // because nothing here knew what it meant.
    expect(
      screen.getByText("OPERATION_TYPE_FUTURES_VARIATION_MARGIN"),
    ).toBeInTheDocument();
    expect(screen.getByText("Вариационная маржа")).toBeInTheDocument();
    expect(screen.getByText("04.08.2026, 12:15")).toBeInTheDocument();
  });

  it("gives every reason its own wording rather than one catch-all", async () => {
    serve([
      makeOperation({ id: "u-1", reason: "instrument_unresolved" }),
      makeOperation({ id: "u-2", reason: "unrepresentable_amount" }),
      makeOperation({ id: "u-3", reason: "engine_refused" }),
    ]);
    renderList();

    expect(
      await screen.findByText("Бумага не найдена в каталоге инструментов"),
    ).toBeInTheDocument();
    expect(screen.getByText("Сумма точнее минимальной денежной единицы")).toBeInTheDocument();
    expect(screen.getByText("Операцию отклонил движок журнала")).toBeInTheDocument();
  });

  // «Операцию отклонил движок журнала» is the same sentence over a sale with
  // nothing behind it, an amount the journal will not hold, and a transfer whose
  // other leg failed. The detail is the only thing that tells one such row from
  // the next — 134 of the owner's rows carried that code and nothing else, and
  // none of them could be acted on.
  it("prints what refused the row, beside the name of the refusal", async () => {
    serve([
      makeOperation({
        id: "u-1",
        reason: "engine_refused",
        detail: "operation: selling 100 units leaves the position at -40",
      }),
    ]);
    renderList();

    expect(await screen.findByText("Операцию отклонил движок журнала")).toBeInTheDocument();
    expect(
      screen.getByText("operation: selling 100 units leaves the position at -40"),
    ).toBeInTheDocument();
  });

  // A row refused before the server kept details carries none, which is an
  // ordinary state rather than a fault: the name of the reason stands on its
  // own, and a blank line under it would read as something still loading.
  //
  // Asserted on the SHAPE of the cell and not on its text, because the defect
  // this pins — rendering the detail unconditionally — puts an EMPTY element on
  // the screen, and no query by text can see one.
  it("adds nothing under the reason when nothing was written down", async () => {
    serve([makeOperation({ id: "u-1", reason: "engine_refused", detail: "" })]);
    renderList();

    const reason = await screen.findByText("Операцию отклонил движок журнала");
    const stack = reason.parentElement;
    // The reason itself and the broker's own document; no third line.
    expect(stack?.children).toHaveLength(2);
    expect(stack?.lastElementChild?.tagName).toBe("DETAILS");
  });

  // An amount finer than a minor unit is one of the reasons a row is on this
  // list at all, so rounding it for display would erase the evidence.
  it("prints the broker's amount exactly as it arrived", async () => {
    serve([
      makeOperation({ payment: "0.123456789", currency: "USD", reason: "unrepresentable_amount" }),
    ]);
    renderList();

    expect(await screen.findByText("0.123456789 USD")).toBeInTheDocument();
  });

  it("keeps the broker's own record of the operation available", async () => {
    serve([makeOperation()]);
    renderList();

    expect(await screen.findByText("Что прислал брокер")).toBeInTheDocument();
    expect(screen.getByText(/broker-op-1/)).toBeInTheDocument();
  });

  // Empty means nothing on this list — which is also true of a connection that
  // has never read a single operation, where «все операции разобраны» would be
  // a claim about work that was never done.
  it("says there are none, not that everything was understood", async () => {
    serve([]);
    renderList();

    expect(await screen.findByText("Неразобранных операций нет")).toBeInTheDocument();
    // «Брокер ИХ отдал, программа сохранила ИХ как есть» is a sentence about
    // rows, and over an empty list «их» has no subject — it was printed
    // directly above «Неразобранных операций нет», which answers it.
    expect(
      screen.queryByText(
        "Брокер их отдал, программа сохранила их как есть — но записями журнала они не стали: ни позиции, ни прибыль их не учитывают",
      ),
    ).not.toBeInTheDocument();
  });

  it("introduces the list where there is a list to introduce", async () => {
    serve([makeOperation({ id: "u-1" })]);
    renderList();

    expect(
      await screen.findByText(
        "Брокер их отдал, программа сохранила их как есть — но записями журнала они не стали: ни позиции, ни прибыль их не учитывают",
      ),
    ).toBeInTheDocument();
  });

  it("fetches the next page on demand", async () => {
    servePages([
      { operations: [makeOperation({ id: "u-1", payment: "-1" })], has_more: true },
      { operations: [makeOperation({ id: "u-2", payment: "-2" })], has_more: false },
    ]);
    renderList();

    fireEvent.click(await screen.findByRole("button", { name: "Показать ещё" }));

    expect(await screen.findByText("-2 RUB")).toBeInTheDocument();
    expect(screen.getByText("-1 RUB")).toBeInTheDocument();
  });
});
