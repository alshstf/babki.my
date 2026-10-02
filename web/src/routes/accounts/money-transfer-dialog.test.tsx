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
