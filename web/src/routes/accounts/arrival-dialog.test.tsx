import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { ArrivalDialog } from "./arrival-dialog";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const account: AccountWithBalance = {
  id: "acc-freedom",
  name: "Freedom KZ",
  type: "brokerage",
  currency: "USD",
  institution: "Freedom Finance",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  trades_abroad: false,
  counted_by: "balance",
  balance: undefined,
};

const ko: Instrument = {
  id: "inst-ko",
  type: "share",
  name: "Coca-Cola",
  ticker: "KO",
  isin: "US1912161007",
  figi: "",
  currency: "USD",
  frozen: false,
};

let postStatus = 201;
const posted: unknown[] = [];

function serve() {
  fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
    const request = input instanceof Request ? input : new Request(String(input));
    const path = new URL(request.url, "http://localhost").pathname;
    const json = (status: number, body: unknown) =>
      new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
    if (path.endsWith("/api/v1/instruments")) return json(200, { instruments: [ko], has_more: false });
    if (path.endsWith("/api/v1/operations/arrivals")) {
      posted.push(await request.json());
      return postStatus === 201 ? json(201, { id: "op-new" }) : json(postStatus, { error: "inconsistent" });
    }
    return json(404, null);
  });
}

async function openWith(quantity: string) {
  serve();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ArrivalDialog open onOpenChange={() => {}} account={account} />
    </QueryClientProvider>,
  );
  fireEvent.click(await screen.findByRole("button", { name: /Coca-Cola/ }));
  fireEvent.change(document.getElementById("arrival-qty")!, { target: { value: quantity } });
  fireEvent.change(document.getElementById("arrival-date")!, { target: { value: "2026-06-15" } });
}

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
  posted.length = 0;
  postStatus = 201;
});

describe("ArrivalDialog", () => {
  it("records shares from another broker without a price, saying they will count as bought for nothing", async () => {
    await openWith("10");
    expect(screen.getByText(/купленными за 0/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({
      account_id: "acc-freedom",
      instrument_id: "inst-ko",
      occurred_on: "2026-06-15",
      quantity: "10",
      currency: "USD",
      note: "",
      purchases: [],
    });
  });

  it("records them with their purchases when the owner knows them", async () => {
    await openWith("10");
    fireEvent.click(screen.getByLabelText(/Я знаю цену покупки/));
    fireEvent.change(document.getElementById("new-arrival-0-quantity")!, { target: { value: "10" } });
    fireEvent.change(document.getElementById("new-arrival-0-price")!, { target: { value: "55" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toMatchObject({
      quantity: "10",
      purchases: [{ quantity: "10", price: "55", cost_minor: null, fee_minor: 0, acquired_on: null }],
    });
  });

  it("will not send purchases that are not the shares that arrived", async () => {
    await openWith("10");
    fireEvent.click(screen.getByLabelText(/Я знаю цену покупки/));
    fireEvent.change(document.getElementById("new-arrival-0-quantity")!, { target: { value: "8" } });
    fireEvent.change(document.getElementById("new-arrival-0-price")!, { target: { value: "55" } });
    expect(screen.getByRole("button", { name: "Сохранить" })).toBeDisabled();
  });

  it("says the journal refused the arrival, by the status alone", async () => {
    postStatus = 409;
    await openWith("10");
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    expect(await screen.findByText(/не сходится журнал счёта/)).toBeInTheDocument();
    expect(screen.queryByText(/inconsistent/)).not.toBeInTheDocument();
  });
});
