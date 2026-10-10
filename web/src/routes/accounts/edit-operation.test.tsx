import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { CashDialog } from "./cash-dialog";
import { TradeDialog } from "./trade-dialog";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";
import { editDialogOf, type Operation } from "@/api/operations";
import { minorToInput } from "@/lib/money";

// What was sent: method, path and body of every request the dialogs made.
const sent = vi.hoisted(() => [] as { method: string; url: string; body: unknown }[]);
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});
fetchMock.mockImplementation(async (input: Request) => {
  if (input.method === "GET") {
    // The instrument picker's catalog: nothing to list.
    return new Response(JSON.stringify({ instruments: [], has_more: false }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  }
  const body = await input.clone().json();
  sent.push({ method: input.method, url: input.url, body });
  return new Response(JSON.stringify({ ...(body as object), id: "op-1", source: "manual" }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
});

afterEach(() => {
  cleanup();
  sent.length = 0;
});

const account: AccountWithBalance = {
  id: "acc-1",
  name: "Брокерский",
  type: "brokerage",
  currency: "RUB",
  institution: "",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  trades_abroad: false,
  counted_by: "balance",
};

const sber: Instrument = {
  id: "instr-share",
  type: "share",
  name: "Сбербанк",
  ticker: "SBER",
  isin: "RU0009029540",
  figi: "",
  currency: "RUB",
  frozen: false,
};

function operation(overrides: Partial<Operation>): Operation {
  return {
    id: "op-7",
    account_id: "acc-1",
    type: "buy",
    occurred_on: "2026-07-10",
    amount_minor: -300_000,
    currency: "RUB",
    fee_minor: 0,
    note: "",
    source: "manual",
    created_at: "2026-07-10T10:00:00Z",
    has_undated_lots: false,
    assembled_from_lots: false,
    ...overrides,
  } as Operation;
}

function wrap(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

describe("editing a recorded operation", () => {
  it("opens the cash dialog on what was recorded and saves it in place", async () => {
    wrap(
      <CashDialog
        open
        onOpenChange={() => {}}
        account={account}
        editing={operation({ type: "withdrawal", amount_minor: -1_250_050, note: "на отпуск" })}
      />,
    );
    expect(screen.getByText("Изменить операцию")).toBeTruthy();
    expect((screen.getByLabelText(/Сумма/) as HTMLInputElement).value).toBe("12500.50");
    expect((screen.getByLabelText(/Комментарий|Заметка|Примечание/) as HTMLInputElement).value).toBe(
      "на отпуск",
    );
    expect(screen.getByRole("combobox", { name: "Тип операции" })).toBeDisabled();

    fireEvent.change(screen.getByLabelText(/Сумма/), { target: { value: "12000" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].method).toBe("PUT");
    expect(sent[0].url).toMatch(/\/api\/v1\/operations\/op-7$/);
    expect(sent[0].body).toMatchObject({ type: "withdrawal", amount_minor: -1_200_000, occurred_on: "2026-07-10" });
  });

  it("opens the trade dialog on the recorded trade, paper included", async () => {
    wrap(
      <TradeDialog
        open
        onOpenChange={() => {}}
        account={account}
        side="buy"
        editing={operation({ quantity: "10", price: "300", fee_minor: 1_500, instrument_id: sber.id })}
        editingInstrument={sber}
      />,
    );
    expect(screen.getByText("Изменить операцию")).toBeTruthy();
    expect((screen.getByLabelText(/Количество/) as HTMLInputElement).value).toBe("10");
    expect((screen.getByLabelText(/Комиссия/) as HTMLInputElement).value).toBe("15");

    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].method).toBe("PUT");
    expect(sent[0].body).toMatchObject({ instrument_id: sber.id, quantity: "10", price: "300", fee_minor: 1_500 });
  });

  it("a new entry still goes in as a new one", async () => {
    wrap(<CashDialog open onOpenChange={() => {}} account={account} />);
    fireEvent.change(screen.getByLabelText(/Сумма/), { target: { value: "100" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].method).toBe("POST");
    expect(sent[0].url).toMatch(/\/api\/v1\/operations$/);
  });
});

describe("which operations are edited in place", () => {
  it("offers the dialog a row was entered in, and nothing for what the server would refuse", () => {
    expect(editDialogOf(operation({ type: "buy" }))).toBe("trade");
    expect(editDialogOf(operation({ type: "tax" }))).toBe("cash");
    expect(editDialogOf(operation({ type: "coupon" }))).toBe("income");
    expect(editDialogOf(operation({ type: "buy", source: "tinvest" }))).toBeNull();
    expect(editDialogOf(operation({ type: "transfer_in" }))).toBeNull();
    expect(editDialogOf(operation({ type: "sell", transfer_group_id: "g-1" }))).toBeNull();
    expect(editDialogOf(operation({ type: "split" }))).toBeNull();
    expect(editDialogOf(operation({ type: "buy", source: "csv" }))).toBe("trade");
    expect(editDialogOf(operation({ type: "split", source: "registry" }))).toBeNull();
  });
});

describe("minorToInput", () => {
  it("writes an amount the way an amount field reads it back", () => {
    expect(minorToInput(290_000)).toBe("2900");
    expect(minorToInput(12_345)).toBe("123.45");
    expect(minorToInput(-500)).toBe("5");
    expect(minorToInput(5)).toBe("0.05");
    expect(minorToInput(0)).toBe("0");
    expect(minorToInput(1_005)).toBe("10.05");
  });
});
