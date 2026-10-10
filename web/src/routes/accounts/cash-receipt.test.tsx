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
// Every lookup answers an empty list, but a receipt numbered 777 was written
// already.
fetchMock.mockImplementation((input: Request) => {
  const written = input.url.includes("/operations/receipt") && input.url.includes("fd=777");
  const body = written
    ? [{ id: "op-1", account_id: "card", occurred_on: "2026-10-09", amount_minor: -123450, currency: "RUB" }]
    : [];
  return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
});

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

function open(preset?: "expense" | "income") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <CashDialog open onOpenChange={() => {}} account={card} preset={preset} />
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
