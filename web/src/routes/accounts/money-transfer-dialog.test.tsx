import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { MoneyTransferDialog } from "./money-transfer-dialog";
import type { AccountWithBalance } from "@/api/accounts";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Radix's Select scrolls the highlighted option into view, which jsdom lacks.
Element.prototype.scrollIntoView = () => {};

const source: AccountWithBalance = {
  id: "acc-1",
  name: "Т-Банк",
  type: "brokerage",
  currency: "RUB",
  institution: "",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  trades_abroad: false,
  kept_by_operations: false,
  counted_by: "journal",

};
const dollars: AccountWithBalance = { ...source, id: "acc-2", name: "Долларовый", currency: "USD" };
const archived: AccountWithBalance = { ...source, id: "acc-3", name: "Закрытый", status: "archived" };

let sent: unknown[] = [];

function serve() {
  sent = [];
  fetchMock.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : String(input);
    const path = new URL(url, "http://localhost").pathname;
    const json = (status: number, body: unknown) =>
      new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
    if (path.endsWith("/api/v1/accounts")) return json(200, [source, dollars, archived]);
    if (path.endsWith("/api/v1/operations/money-transfer")) {
      const text = input instanceof Request ? await input.text() : String(init?.body ?? "");
      sent.push(JSON.parse(text));
      return json(201, { out: {}, in: {} });
    }
    return json(404, null);
  });
}

function renderDialog(onOpenChange = vi.fn()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <MoneyTransferDialog open onOpenChange={onOpenChange} account={source} />
    </QueryClientProvider>,
  );
  return onOpenChange;
}

async function pick(name: string) {
  fireEvent.keyDown(await screen.findByRole("combobox"), { key: "Enter" });
  fireEvent.click(await screen.findByRole("option", { name }));
}

afterEach(cleanup);

describe("MoneyTransferDialog", () => {
  it("sends what left, in this account's currency, to the account picked", async () => {
    serve();
    const onOpenChange = renderDialog();
    await pick("Долларовый");
    fireEvent.change(screen.getByLabelText("Сумма, RUB"), { target: { value: "1 500,50" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toMatchObject({
      from_account_id: "acc-1",
      to_account_id: "acc-2",
      amount_minor: 150_050,
      currency: "RUB",
    });
    expect(sent[0]).not.toHaveProperty("received_minor");
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("sends what arrived when it arrived converted, in the receiving account's currency by default", async () => {
    serve();
    renderDialog();
    await pick("Долларовый");
    fireEvent.change(screen.getByLabelText("Сумма, RUB"), { target: { value: "9000" } });
    fireEvent.click(screen.getByLabelText("Пришло в другой валюте или другой суммой"));
    fireEvent.change(screen.getByLabelText("Пришло, USD"), { target: { value: "100" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toMatchObject({ amount_minor: 900_000, currency: "RUB", received_minor: 10_000, received_currency: "USD" });
  });

  it("offers neither this account nor an archived one, and saves nothing until an account is picked", async () => {
    serve();
    renderDialog();
    fireEvent.keyDown(await screen.findByRole("combobox"), { key: "Enter" });
    expect(await screen.findByRole("option", { name: "Долларовый" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "Т-Банк" })).toBeNull();
    expect(screen.queryByRole("option", { name: "Закрытый" })).toBeNull();
    fireEvent.keyDown(screen.getByRole("option", { name: "Долларовый" }), { key: "Escape" });
    fireEvent.change(screen.getByLabelText("Сумма, RUB"), { target: { value: "100" } });
    expect(screen.getByRole("button", { name: "Сохранить" })).toBeDisabled();
  });
});

// Off a credit card, money moved has no grace and the tariff's fee (decision
// Р-30): cash past what is left free this month, a transfer always.
describe("MoneyTransferDialog off a credit card", () => {
  const card: AccountWithBalance = { ...source, id: "card-1", name: "Кредитка", type: "credit_card" };
  const cash: AccountWithBalance = { ...source, id: "cash-1", name: "Кошелёк", type: "cash" };
  const fees = {
    monthly_minor: 0, yearly_minor: 0, cash_free_minor: 100_000_00, cash_percent: "5.9", cash_fixed_minor: 590_00,
    transfer_free_minor: 0, transfer_percent: "4.9", transfer_fixed_minor: 390_00, intro_days: 0, intro_free_minor: 0,
    penalty_daily_percent: "0.1", penalty_yearly_percent: "0", penalty_from_day: 0,
  };
  const status = { cash_this_period_minor: 80_000_00, transfers_this_period_minor: 0, intro_left_minor: 0, intro_until: null };

  it("tells the fee before the money leaves", async () => {
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const url = input instanceof Request ? input.url : String(input);
      const path = new URL(url, "http://localhost").pathname;
      const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
      if (path.endsWith("/api/v1/accounts")) return json([card, cash, source]);
      if (path.endsWith("/card-1/credit-card")) return json({ terms: { fees }, status });
      return new Response("null", { status: 404 });
    });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MoneyTransferDialog open onOpenChange={vi.fn()} account={card} />
      </QueryClientProvider>,
    );
    await pick("Кошелёк");
    fireEvent.change(screen.getByLabelText("Сумма, RUB"), { target: { value: "30000" } });
    const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");
    await waitFor(() => expect(norm(screen.getByTestId("card-transfer-warning").textContent)).toContain("ещё 20 000,00 ₽"));
    // 10 000 past the free part: 590 and 590.
    expect(norm(screen.getByTestId("card-transfer-warning").textContent)).toContain("≈ 1 180,00 ₽");

    await pick("Т-Банк");
    await waitFor(() => expect(norm(screen.getByTestId("card-transfer-warning").textContent)).toContain("≈ 1 860,00 ₽"));
    expect(screen.getByTestId("card-transfer-warning").textContent).toContain("льготы нет");
  });

  // Альфа's «без % на всё» (#460): the money moved keeps the grace.
  it("tells the grace when the card gives it to transfers", async () => {
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const url = input instanceof Request ? input.url : String(input);
      const path = new URL(url, "http://localhost").pathname;
      const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
      if (path.endsWith("/api/v1/accounts")) return json([card, cash, source]);
      if (path.endsWith("/card-1/credit-card")) return json({ terms: { fees, grace_moves: true }, status });
      return new Response("null", { status: 404 });
    });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MoneyTransferDialog open onOpenChange={vi.fn()} account={card} />
      </QueryClientProvider>,
    );
    await pick("Т-Банк");
    await waitFor(() => expect(screen.getByTestId("card-transfer-warning").textContent).toContain("в льготе, как покупки"));
    expect(screen.getByTestId("card-transfer-warning").textContent).not.toContain("льготы нет");
  });

  // Т-Банк's free transfers of the month and ВТБ's first days (#462).
  it("counts the free transfers and the first days", async () => {
    const tbank = { ...fees, transfer_free_minor: 80_000_00 };
    let st: Record<string, unknown> = { ...status, transfers_this_period_minor: 60_000_00 };
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const url = input instanceof Request ? input.url : String(input);
      const path = new URL(url, "http://localhost").pathname;
      const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
      if (path.endsWith("/api/v1/accounts")) return json([card, cash, source]);
      if (path.endsWith("/card-1/credit-card")) return json({ terms: { fees: tbank }, status: st });
      return new Response("null", { status: 404 });
    });
    const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");
    const open = async () => {
      const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      render(
        <QueryClientProvider client={qc}>
          <MoneyTransferDialog open onOpenChange={vi.fn()} account={card} />
        </QueryClientProvider>,
      );
      await pick("Т-Банк");
      fireEvent.change(screen.getByLabelText("Сумма, RUB"), { target: { value: "30000" } });
    };
    await open();
    await waitFor(() => expect(norm(screen.getByTestId("card-transfer-warning").textContent)).toContain("перевести ещё 20 000,00 ₽"));
    // 10 000 past the free part: 490 and 390.
    expect(norm(screen.getByTestId("card-transfer-warning").textContent)).toContain("≈ 880,00 ₽");
    cleanup();

    st = { ...status, transfers_this_period_minor: 60_000_00, intro_left_minor: 50_000_00, intro_until: "2026-09-30" };
    await open();
    await waitFor(() => expect(norm(screen.getByTestId("card-transfer-warning").textContent)).toContain("До 30.09.2026 снятие и переводы без комиссии — ещё на 50 000,00 ₽."));
    expect(screen.getByTestId("card-transfer-warning").textContent).not.toContain("комиссию ≈");
  });
});
