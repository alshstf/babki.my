import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { BalanceDialog } from "./balance-dialog";
import type { AccountWithBalance } from "@/api/accounts";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call: a body can be read only once.
function serve(status: number, body: unknown) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );
}

beforeEach(() => {
  serve(200, null);
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
  counted_by: "balance",
  balance: { as_of: "2026-07-20", amount_minor: 1_000_000 },
};

function open() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <BalanceDialog open onOpenChange={() => {}} account={account} />
    </QueryClientProvider>,
  );
}

const amountField = () => screen.getByLabelText(/Сумма/);
const dateField = () => screen.getByLabelText("На дату");
const saveButton = () => screen.getByRole("button", { name: "Сохранить" });

function typeAmount(value: string) {
  fireEvent.change(amountField(), { target: { value } });
}

afterEach(() => {
  cleanup();
  fetchMock.mockClear();
});

// #89: the field bounded nothing, and an oversized balance made the
// accounts screen answer 500. The server refuses now; the field refuses at
// the keystroke and says which problem it is.
describe("BalanceDialog: a sum too large to record", () => {
  it("does not send it, and says it is too large rather than unreadable", () => {
    open();
    typeAmount("10000000000000,01"); // one kopeck past the bound

    expect(saveButton()).toBeDisabled();
    // The number parses, so the parse error would be the wrong cause.
    expect(screen.queryByText(/Не удалось разобрать сумму/)).toBeNull();
    expect(screen.getByText(/Слишком большая сумма/)).toBeTruthy();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("names the largest sum it would take, in the account's currency", () => {
    open();
    typeAmount("10000000000000,01");

    // Ten trillion roubles as this screen writes money, not raw kopecks. \s,
    // not a space: Intl uses non-breaking and narrow spaces.
    const hint = (screen.getByText(/Слишком большая сумма/).textContent ?? "").replace(/\s/g, " ");
    expect(hint).toContain("10 000 000 000 000 ₽");
  });

  it("still calls unreadable text unreadable", () => {
    open();
    typeAmount("abc");

    expect(saveButton()).toBeDisabled();
    expect(screen.getByText(/Не удалось разобрать сумму/)).toBeTruthy();
    expect(screen.queryByText(/Слишком большая сумма/)).toBeNull();
  });

  it("takes the largest sum there is, and complains about nothing", () => {
    open();
    typeAmount("10000000000000");

    expect(saveButton()).not.toBeDisabled();
    expect(screen.queryByText(/Слишком большая сумма/)).toBeNull();
    expect(screen.queryByText(/Не удалось разобрать сумму/)).toBeNull();
  });

  it("takes an ordinary balance", () => {
    open();
    typeAmount("150 000,50");

    expect(saveButton()).not.toBeDisabled();
    expect(screen.queryByText(/Слишком большая сумма/)).toBeNull();
  });
});

// #95: the server's English refusal was printed as is. The date field's
// `max` only limits the picker arrows.
describe("BalanceDialog: a date the account cannot have had", () => {
  it("does not send a date in the future, and says why at the field", () => {
    open();
    typeAmount("1000");
    fireEvent.change(dateField(), { target: { value: "2099-01-01" } });

    expect(saveButton()).toBeDisabled();
    expect(screen.getByText("Дата не может быть в будущем")).toBeTruthy();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("says nothing about the date the dialog opened on", () => {
    open();
    typeAmount("1000");

    expect(saveButton()).not.toBeDisabled();
    expect(screen.queryByText("Дата не может быть в будущем")).toBeNull();
  });
});

describe("BalanceDialog: a refusal that came back anyway", () => {
  it("says it in Russian and does not repeat the server's own words", async () => {
    serve(400, { error: "as_of must not be in the future" });
    open();
    typeAmount("1000");
    fireEvent.click(saveButton());

    expect(await screen.findByText("Не удалось сохранить баланс")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("as_of");
  });
});
