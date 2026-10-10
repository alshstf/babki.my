import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
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

// A fresh Response per call: a body can be read only once.
fetchMock.mockImplementation(() =>
  Promise.resolve(
    new Response("null", { status: 200, headers: { "Content-Type": "application/json" } }),
  ),
);

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
      <CashDialog open onOpenChange={() => {}} account={account} />
    </QueryClientProvider>,
  );
}

const amountField = () => screen.getByLabelText(/Сумма/);
const saveButton = () => screen.getByRole("button", { name: "Сохранить" });

function typeAmount(value: string) {
  fireEvent.change(amountField(), { target: { value } });
}

afterEach(() => {
  cleanup();
  fetchMock.mockClear();
});

// The balance field's bound (MAX_AMOUNT_MINOR) with the right refusal:
// «Введите положительную сумму» is false of a positive, parseable sum.
describe("CashDialog: a sum too large to record", () => {
  it("says it is too large rather than asking for a positive number", () => {
    open();
    typeAmount("20000000000000"); // twice the bound, and positive

    expect(saveButton()).toBeDisabled();
    expect(screen.getByText(/Слишком большая сумма/)).toBeTruthy();
    expect(screen.queryByText(/Введите положительную сумму/)).toBeNull();
    // Only the categories are read; nothing is written.
    expect(
      fetchMock.mock.calls.filter(([input]) => input instanceof Request && input.method !== "GET"),
    ).toHaveLength(0);
  });

  it("names the largest sum it would take, in the account's currency", () => {
    open();
    typeAmount("10000000000000,01"); // one kopeck past the bound

    // Ten trillion roubles as this screen writes money, not raw kopecks. \s,
    // not a space: Intl uses non-breaking and narrow spaces.
    const hint = (screen.getByText(/Слишком большая сумма/).textContent ?? "").replace(/\s/g, " ");
    expect(hint).toContain("10 000 000 000 000 ₽");
  });

  it("still asks for a positive number when the text is not one", () => {
    open();
    typeAmount("abc");

    expect(saveButton()).toBeDisabled();
    expect(screen.getByText(/Введите положительную сумму/)).toBeTruthy();
    expect(screen.queryByText(/Слишком большая сумма/)).toBeNull();
  });

  it("takes the largest sum there is, and complains about nothing", () => {
    open();
    typeAmount("10000000000000");

    expect(saveButton()).not.toBeDisabled();
    expect(screen.queryByText(/Слишком большая сумма/)).toBeNull();
    expect(screen.queryByText(/Введите положительную сумму/)).toBeNull();
  });

  it("takes an ordinary deposit", () => {
    open();
    typeAmount("150 000,50");

    expect(saveButton()).not.toBeDisabled();
    expect(screen.queryByText(/Слишком большая сумма/)).toBeNull();
  });
});

// Enter saves what the button would, and only from a text field.
describe("CashDialog: Enter", () => {
  const posts = () =>
    fetchMock.mock.calls.filter(([input]) => input instanceof Request && input.method === "POST").length;

  it("saves a valid deposit from the amount field", async () => {
    open();
    typeAmount("1500");
    fireEvent.keyDown(amountField(), { key: "Enter" });
    await vi.waitFor(() => expect(posts()).toBe(1));
  });

  it("saves nothing while the amount is not a sum", async () => {
    open();
    typeAmount("abc");
    fireEvent.keyDown(amountField(), { key: "Enter" });
    await new Promise((r) => setTimeout(r, 20));
    expect(posts()).toBe(0);
  });

  it("saves nothing on Shift+Enter, or on Enter outside a text field", async () => {
    open();
    typeAmount("1500");
    fireEvent.keyDown(amountField(), { key: "Enter", shiftKey: true });
    fireEvent.keyDown(saveButton(), { key: "Enter" });
    await new Promise((r) => setTimeout(r, 20));
    expect(posts()).toBe(0);
  });
});
