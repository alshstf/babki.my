import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { QuickAdd } from "./quick-add";
import type { AccountWithBalance } from "@/api/accounts";
import type { Receipt } from "@/api/receipts";

// openapi-fetch captures globalThis.fetch at import time, and the quick add
// reads the address as its bundle loads: both are set with vi.hoisted, ahead
// of the imports — as the app is started by a photo shared to it.
const fetchMock = vi.hoisted(() => {
  window.history.replaceState(null, "", "/?add&receipt=r-9");
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const card: AccountWithBalance = {
  id: "card", name: "Кредитка Альфа", type: "credit_card", currency: "RUB", institution: "", status: "active",
  created_at: "2026-01-01T00:00:00Z", valued_by_balance: false, trades_abroad: false, kept_by_operations: true,
  counted_by: "journal", balance: { as_of: "2026-10-01", amount_minor: 0 },
};

const shared: Receipt = {
  id: "r-9", operation_id: null, fn: "7380440700123456", fd: "51243", fp: "1234567890", kind: "purchase",
  issued_at: "2026-10-09T19:15", total_minor: 234_090, seller: null, seller_inn: null, address: null, items: [], source: "qr",
};

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("QuickAdd started by a shared receipt", () => {
  it("opens filled from the receipt, and the row saved completes it", async () => {
    fetchMock.mockImplementation((input: Request) => {
      const url = new URL(input.url);
      const json = (body: unknown, status = 200) =>
        Promise.resolve(new Response(body === null ? null : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
      if (url.pathname.endsWith("/accounts")) return json([card]);
      if (url.pathname.endsWith("/receipts/match")) return json({ receipt: shared, written_to: null, candidates: [] });
      if (url.pathname.endsWith("/receipts")) return json([shared]);
      if (url.pathname.endsWith("/receipts/r-9/operation")) return json(null, 204);
      if (input.method === "POST") {
        return input.clone().json().then((body: Record<string, unknown>) =>
          json({ ...body, id: "op-1", source: "manual", created_at: "2026-10-10T00:00:00Z" }, 201),
        );
      }
      return json([]);
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <QuickAdd />
      </QueryClientProvider>,
    );
    await waitFor(() => expect((screen.getByLabelText(/Сумма, RUB/) as HTMLInputElement).value).toBe("2340.90"));
    expect((screen.getByLabelText("Дата") as HTMLInputElement).value).toBe("2026-10-09");
    expect(screen.getByTestId("receipt-already").textContent).toMatch(/ждёт траты/);
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(new URL(put.url).pathname).toMatch(/\/receipts\/r-9\/operation$/);
    expect(await put.json()).toEqual({ operation_id: "op-1" });
  });
});
