import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { TransferDialog } from "./transfer-dialog";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// jsdom lacks scrollIntoView, which Radix Select calls on open; stubbed
// here so no other file's environment changes.
Element.prototype.scrollIntoView = () => {};

// What POST /api/v1/operations/transfer answers; only the status may be
// read (see isConflict).
let transferStatus = 201;

const source: AccountWithBalance = {
  id: "acc-1",
  name: "Брокерский",
  type: "brokerage",
  currency: "RUB",
  institution: "Broker Co",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  trades_abroad: false,
  kept_by_operations: false,
  counted_by: "balance",
  balance: { as_of: "2026-07-20", amount_minor: 1_000_000 },
};

const target: AccountWithBalance = { ...source, id: "acc-2", name: "ИИС" };

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

function serve() {
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = input instanceof Request ? input.url : String(input);
    const path = new URL(url, "http://localhost").pathname;
    const json = (status: number, body: unknown) =>
      Promise.resolve(
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
      );
    if (path.endsWith("/api/v1/instruments")) return json(200, { instruments: [sber], has_more: false });
    if (path.endsWith("/api/v1/accounts")) return json(200, [source, target]);
    if (path.endsWith("/api/v1/operations/transfer")) {
      if (transferStatus !== 201) {
        // The server's English log prose.
        return json(transferStatus, {
          error: "journal would become inconsistent: not enough quantity: have 4, need 10",
        });
      }
      return json(201, { out: {}, in: {} });
    }
    return json(404, null);
  });
}

async function openAndSubmit() {
  serve();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TransferDialog open onOpenChange={() => {}} account={source} />
    </QueryClientProvider>,
  );
  fireEvent.click(await screen.findByRole("button", { name: /Сбербанк/ }));
  // Radix's Select opens on pointerdown, which jsdom lacks; Enter opens it
  // too.
  fireEvent.keyDown(screen.getByRole("combobox"), { key: "Enter" });
  fireEvent.click(await screen.findByRole("option", { name: "ИИС" }));
  fireEvent.change(screen.getByLabelText(/Количество/), { target: { value: "10" } });
  fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
  return screen.findByRole("alert");
}

beforeEach(() => {
  fetchMock.mockReset();
  transferStatus = 201;
});

afterEach(() => {
  cleanup();
});

// #23 again: a 409 means a replay refused, and here not even which of the
// two accounts.
describe("TransferDialog: a journal the server would not replay", () => {
  it("says a journal did not add up, and never that the source is short of securities", async () => {
    transferStatus = 409;
    const alert = await openAndSubmit();

    expect(alert.textContent).toContain("с ним не сошёлся журнал одного из двух счетов");
    // The sentence this replaces, in the words the dictionary held.
    expect(document.body.textContent).not.toContain("Недостаточно бумаг для переноса");
    // And still not the server's own English, which is a log line (#95).
    expect(document.body.textContent).not.toContain("not enough quantity");
  });

  it("says only that something went wrong when the refusal is not a conflict", async () => {
    transferStatus = 500;
    const alert = await openAndSubmit();

    expect(alert.textContent).toContain("Что-то пошло не так");
    expect(document.body.textContent).not.toContain("не сошёлся журнал");
  });
});
