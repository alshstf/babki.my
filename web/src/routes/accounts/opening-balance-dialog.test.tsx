import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { OpeningBalanceDialog } from "./opening-balance-dialog";
import type { AccountWithBalance } from "@/api/accounts";
import type { CashPosition } from "@/api/positions";
import type { TinvestConnection } from "@/api/connections";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const account: AccountWithBalance = {
  id: "acc-1",
  name: "Брокерский",
  type: "brokerage",
  currency: "RUB",
  institution: "Broker Co",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  trades_abroad: false,
  counted_by: "journal",
};

// The memo's example: 300 000 ₽ of papers bought on 2 March and 4 000 ₽ of
// dividends, with no deposit in the journal.
const short: CashPosition & { overdrawn_since: string } = {
  currency: "RUB",
  amount_minor: -29_600_000,
  overdrawn_since: "2026-03-02",
  in_base: {
    currency: "RUB",
    value_minor: -29_600_000,
    cost_minor: 0,
    unrealized_pnl_minor: null,
    realized_pnl_minor: 0,
    gap: "negative_balance",
  },
};

function linked(broker: string, unparsed = 0): TinvestConnection[] {
  return [
    {
      id: "conn-1",
      status: "active",
      token_last4: "abcd",
      accounts: [],
      last_successful_sync_at: null,
      reconciles: [
        {
          link_id: "link-1",
          account_id: account.id,
          broker_account_name: "Брокерский",
          at: "2026-10-06T09:30:00Z",
          status: "mismatched",
          mismatches: [{ kind: "currency", label: "RUB", broker, journal: "-296000" }],
          currency_trades_unparsed: unparsed,
        },
      ],
    } as unknown as TinvestConnection,
  ];
}

const posted: Record<string, unknown>[] = [];

function serve(connections: TinvestConnection[]) {
  fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
    const request = input instanceof Request ? input : new Request(String(input));
    const path = new URL(request.url, "http://localhost").pathname;
    const json = (status: number, body: unknown) =>
      new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
    if (path.endsWith("/api/v1/tinvest/connections")) return json(200, { connections });
    if (path.endsWith("/api/v1/operations")) {
      posted.push(await request.json());
      return json(201, { id: "op-new", account_id: account.id });
    }
    return json(404, null);
  });
}

function open(connections: TinvestConnection[] = []) {
  serve(connections);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <OpeningBalanceDialog open onOpenChange={() => {}} account={account} money={short} />
    </QueryClientProvider>,
  );
}

const heldField = () => screen.getByLabelText(/Сколько RUB на счёте сейчас/) as HTMLInputElement;
const saveButton = () => screen.getByRole("button", { name: "Записать пополнение" });
// Intl writes non-breaking and narrow spaces.
const text = (el: HTMLElement) => (el.textContent ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
  posted.length = 0;
});

describe("OpeningBalanceDialog", () => {
  it("records one deposit of what is held less what the journal counts, on the day it first went short", async () => {
    open();
    fireEvent.change(heldField(), { target: { value: "12 500" } });

    expect(text(screen.getByTestId("opening-preview"))).toContain("Запишем пополнение 308 500,00 ₽ датой 02.03.2026");
    fireEvent.click(saveButton());

    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toMatchObject({
      account_id: "acc-1",
      type: "deposit",
      occurred_on: "2026-03-02",
      amount_minor: 30_850_000,
      currency: "RUB",
    });
    expect(String(posted[0].note)).toMatch(/^Начальный остаток: на счёте 12\s500,00\s₽ на /);
  });

  it("takes nought: an account may hold no money today", () => {
    open();
    fireEvent.change(heldField(), { target: { value: "0" } });

    expect(saveButton()).toBeEnabled();
    expect(text(screen.getByTestId("opening-preview"))).toContain("296 000,00 ₽");
  });

  it("refuses a negative or unreadable sum", () => {
    open();
    for (const value of ["-5", "abc", ""]) {
      fireEvent.change(heldField(), { target: { value } });
      expect(saveButton()).toBeDisabled();
      expect(screen.queryByTestId("opening-preview")).toBeNull();
    }
  });

  it("fills in what the broker's last check found, cut to hundredths", async () => {
    open(linked("12500.4567"));

    await waitFor(() => expect(heldField().value).toBe("12500.45"));
    expect(screen.getByTestId("opening-from-broker")).toBeTruthy();
    expect(screen.queryByTestId("opening-unparsed")).toBeNull();
  });

  it("warns when the broker's currency trades are not imported, since they move money too", async () => {
    open(linked("12500", 2));

    expect(text(await screen.findByTestId("opening-unparsed"))).toContain("(их 2)");
  });

  it("fills in nothing for an account the broker check does not cover", async () => {
    open([]);

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(heldField().value).toBe("");
    expect(screen.queryByTestId("opening-from-broker")).toBeNull();
  });
});
