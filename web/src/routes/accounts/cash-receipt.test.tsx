import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { CashDialog } from "./cash-dialog";
import type { AccountWithBalance } from "@/api/accounts";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});
// The server as the dialog meets it: a receipt numbered 777 is written
// already, one numbered 888 finds the bank's row of its purchase, any other
// is new; a row saved and a receipt recorded answer as created.
const json = (body: unknown, status = 200) =>
  Promise.resolve(new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
const bankRow = { id: "op-bank", account_id: "card", occurred_on: "2026-10-10", amount_minor: -123450, currency: "RUB" };
fetchMock.mockImplementation((input: Request) => {
  const url = new URL(input.url);
  if (url.pathname.endsWith("/receipts/match")) {
    const fd = url.searchParams.get("fd");
    if (fd === "777") return json({ receipt: { id: "r-1" }, written_to: { ...bankRow, occurred_on: "2026-10-09" }, candidates: [] });
    return json({ receipt: null, written_to: null, candidates: fd === "888" ? [bankRow] : [] });
  }
  if (url.pathname.endsWith("/receipts") && input.method === "POST") return json({ id: "r-2" }, 201);
  if (url.pathname.endsWith("/operations") && input.method === "POST") return json({ id: "op-new", account_id: "card" }, 201);
  if (url.pathname.endsWith("/accounts")) return json([card]);
  return json([]);
});
const posted = (path: string) =>
  fetchMock.mock.calls.map(([r]) => r as Request).filter((r) => r.method === "POST" && new URL(r.url).pathname.endsWith(path));

// The photo is not read in jsdom (no canvas): the reader answers what the
// camera would have found.
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

const decoded = vi.hoisted(() => ({ text: null as string | null }));
vi.mock("@/lib/qr-decode", () => ({ decodeQrFromImage: () => Promise.resolve(decoded.text) }));

const card: AccountWithBalance = {
  id: "card",
  name: "Кредитка Альфа",
  type: "credit_card",
  currency: "RUB",
  institution: "",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  trades_abroad: false,
  kept_by_operations: true,
  counted_by: "journal",
  balance: { as_of: "2026-07-20", amount_minor: 0 },
};

const onOpenChange = vi.fn();

function open(preset?: "expense" | "income") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <CashDialog open onOpenChange={onOpenChange} account={card} preset={preset} />
    </QueryClientProvider>,
  );
}

const snap = () =>
  fireEvent.change(screen.getByTestId("receipt-photo"), {
    target: { files: [new File(["x"], "receipt.jpg", { type: "image/jpeg" })] },
  });

afterEach(() => {
  cleanup();
  decoded.text = null;
  onOpenChange.mockClear();
  fetchMock.mockClear();
});

describe("CashDialog: a receipt's QR code", () => {
  it("fills the total, the day and the fiscal numbers from a purchase", async () => {
    decoded.text = "t=20261009T1915&s=1234.50&fn=7380440700000000&i=12345&fp=1234567890&n=1";
    open("expense");
    snap();
    await waitFor(() => expect(screen.getByLabelText(/Сумма/)).toHaveValue("1234.50"));
    expect(screen.getByLabelText("Дата")).toHaveValue("2026-10-09");
    expect(screen.getByLabelText("Заметка")).toHaveValue("Чек 09.10.2026 19:15, ФН 7380440700000000, ФД 12345");
    expect(screen.getByRole("combobox", { name: "Тип операции" }).textContent).toBe("вывод");
    expect(screen.getByTestId("receipt-status").textContent).toMatch(/Продавца в QR-коде нет/);
  });

  it("warns when the same receipt was written already", async () => {
    decoded.text = "t=20261009T1915&s=1234.50&fn=7380440700000000&i=777&fp=1234567890&n=1";
    open("expense");
    snap();
    expect(norm((await screen.findByTestId("receipt-already")).textContent ?? "")).toBe(
      "Похоже, этот чек уже записан: 09.10.2026, 1 234,50 ₽. Проверьте, чтобы не записать его дважды.",
    );
  });

  it("offers the bank's row of the same purchase and completes it", async () => {
    decoded.text = "t=20261009T1915&s=1234.50&fn=7380440700000000&i=888&fp=1234567890&n=1";
    open("expense");
    snap();
    const box = await screen.findByTestId("receipt-candidates");
    await waitFor(() => expect(norm(box.textContent ?? "")).toContain("Кредитка Альфа · 10.10.2026 · 1 234,50 ₽"));
    fireEvent.click(screen.getByRole("button", { name: "Дописать чек" }));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(await posted("/receipts")[0].json()).toEqual({
      fn: "7380440700000000", fd: "888", fp: "1234567890", kind: "purchase", total_minor: 123450,
      issued_at: "2026-10-09T19:15", operation_id: "op-bank", source: "qr",
    });
    expect(posted("/operations")).toHaveLength(0);
  });

  it("records the receipt with the row saved", async () => {
    decoded.text = "t=20261009T1915&s=1234.50&fn=7380440700000000&i=12345&fp=1234567890&n=1";
    open("expense");
    snap();
    await waitFor(() => expect(screen.getByLabelText(/Сумма/)).toHaveValue("1234.50"));
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(posted("/receipts")).toHaveLength(1));
    expect(await posted("/receipts")[0].json()).toMatchObject({ fd: "12345", operation_id: "op-new" });
  });

  it("says nothing more of a receipt not written before", async () => {
    decoded.text = "t=20261009T1915&s=1234.50&fn=7380440700000000&i=12345&fp=1234567890&n=1";
    open("expense");
    snap();
    await waitFor(() => expect(screen.getByLabelText(/Сумма/)).toHaveValue("1234.50"));
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByTestId("receipt-already")).toBeNull();
  });

  it("turns a refund into money coming in", async () => {
    decoded.text = "t=20261009T1915&s=500&fn=1&i=2&fp=3&n=2";
    open("expense");
    snap();
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Тип операции" }).textContent).toBe("пополнение"));
  });

  it("says so when the photo has no code, or a code that is not a receipt's", async () => {
    open("expense");
    snap();
    expect(await screen.findByText(/QR-код на снимке не найден/)).toBeTruthy();
    decoded.text = "https://example.org";
    snap();
    expect(await screen.findByText(/не кассового чека/)).toBeTruthy();
    expect(screen.getByLabelText(/Сумма/)).toHaveValue("");
  });

  it("is not offered for a plain money entry", () => {
    open();
    expect(screen.queryByTestId("receipt-photo")).toBeNull();
  });
});
