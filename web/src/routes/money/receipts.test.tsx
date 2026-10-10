import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { ReceiptsCard } from "./receipts";
import { ReceiptLine } from "@/routes/accounts/receipt-line";
import type { Receipt } from "@/api/receipts";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const waiting: Receipt = {
  id: "r-3", operation_id: null, fn: "1", fd: "3", fp: null, kind: "purchase", issued_at: "2026-10-07T09:00",
  total_minor: 99_900, seller: null, seller_inn: null, address: null, items: [], source: "fns",
};

function answer(role: string, list: Receipt[]) {
  fetchMock.mockImplementation((input: Request) => {
    const url = new URL(input.url);
    const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
    if (url.pathname.endsWith("/receipts/import")) return json({ found: 3, attached: 1, waiting: 1, enriched: 1, known: 0, split: 1 });
    if (url.pathname.endsWith("/receipts")) return json(list);
    if (url.pathname.endsWith("/auth/me")) return json({ user: { id: "u", username: "a", display_name: "A" }, role, space: { id: "s", name: "S", base_currency: "RUB" } });
    return json([]);
  });
}

function show(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("ReceiptsCard", () => {
  it("takes a statement and says what it brought", async () => {
    answer("owner", [waiting]);
    show(<ReceiptsCard />);
    const input = await screen.findByTestId("receipts-file");
    const statement = [{ ticket: { document: { receipt: { fiscalDriveNumber: "1", fiscalDocumentNumber: 3 } } } }];
    fireEvent.change(input, { target: { files: [new File([JSON.stringify(statement)], "checks.json", { type: "application/json" })] } });
    const result = await screen.findByTestId("receipts-result");
    expect(norm(result.textContent)).toBe(
      "Чеков в выписке: 3. Дописано к тратам: 1. Получили продавца и позиции: 1. Разделено по категориям: 1. Ждут траты: 1.",
    );
    const post = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "POST")!;
    expect(await post.json()).toEqual(statement);
    expect(norm(screen.getByTestId("receipt-waiting").textContent)).toBe("07.10.2026 09:00 · продавец не указан999,00 ₽");
  });

  it("refuses a file that is not JSON", async () => {
    answer("owner", []);
    show(<ReceiptsCard />);
    fireEvent.change(await screen.findByTestId("receipts-file"), { target: { files: [new File(["<html>"], "x.json")] } });
    expect(await screen.findByText("Это не файл выписки: в нём не JSON.")).toBeTruthy();
    expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "POST")).toBe(false);
  });

  it("is nothing for a viewer with no receipt waiting", async () => {
    answer("viewer", []);
    show(<ReceiptsCard />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByTestId("money-receipts")).toBeNull();
  });
});

describe("ReceiptLine", () => {
  it("names the seller and opens to the lines", () => {
    show(
      <ReceiptLine
        categories={[]}
        canEdit={false}
        receipt={{
          ...waiting, operation_id: "op-1", seller: "ООО «АГРОТОРГ»",
          items: [
            { name: "Молоко", quantity: "1", price_minor: 8999, sum_minor: 8999 },
            { name: "Бананы", quantity: "1.2", price_minor: 13999, sum_minor: 16799 },
          ],
        }}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Чек: ООО «АГРОТОРГ» · позиций: 2/ }));
    expect(norm(screen.getByTestId("operation-receipt-items").textContent)).toBe("Молоко89,99 ₽Бананы × 1.2167,99 ₽");
  });

  it("teaches a line's category and divides the row again", async () => {
    fetchMock.mockImplementation((input: Request) => {
      const url = new URL(input.url);
      const json = (body: unknown, status = 200) =>
        Promise.resolve(new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
      if (url.pathname.endsWith("/category-rules")) return json({ id: "rule-1", category_id: "c-home", field: "item", pattern: "Порошок стиральный", position: 0 }, 201);
      if (url.pathname.endsWith("/split")) return json({ split: true });
      return json([]);
    });
    Element.prototype.scrollIntoView ??= () => {};
    show(
      <ReceiptLine
        categories={[{ id: "c-home", kind: "expense", name: "Дом", parent_id: null, archived: false, position: 1 }]}
        canEdit
        receipt={{ ...waiting, id: "r-1", operation_id: "op-1", items: [{ name: "Порошок стиральный 3кг", quantity: "1", price_minor: 54999, sum_minor: 54999 }] }}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /позиций: 1/ }));
    fireEvent.click(screen.getByRole("button", { name: "Запомнить…" }));
    const box = screen.getByTestId("receipt-teach");
    expect(norm(box.textContent)).toContain("Позиции с «Порошок стиральный» — всегда в эту категорию");
    fireEvent.keyDown(screen.getByRole("combobox"), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "Дом" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => new URL((r as Request).url).pathname.endsWith("/r-1/split"))).toBe(true));
    const rule = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => new URL(r.url).pathname.endsWith("/category-rules"))!;
    expect(await rule.json()).toEqual({ category_id: "c-home", field: "item", pattern: "Порошок стиральный" });
  });
});
