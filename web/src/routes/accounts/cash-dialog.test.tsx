import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { CashDialog } from "./cash-dialog";
import type { AccountWithBalance } from "@/api/accounts";

// The API client captures globalThis.fetch once, when @/api/client is first
// imported (openapi-fetch: `fetch: baseFetch = globalThis.fetch`), so the double
// has to be in place *before* that import — hence vi.hoisted, which runs ahead
// of the import statements above.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call: a single one handed to mockResolvedValue works once
// and then throws, because a body can only be consumed once.
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

// The same bound the balance field carries, on the same screen and in the same
// currency (money.MaxAmountMinor is one number for both — see MAX_AMOUNT_MINOR).
// What is pinned here is not the refusal but WHICH refusal: this field's own
// complaint is «Введите положительную сумму», and 20 000 000 000 000 is
// positive, is a sum, and parses perfectly. A branch that answered it with that
// sentence would name a cause that is not the cause — the mistake this
// repository has been caught by more than once, and the one the whole
// AmountRefusal type exists to prevent.
describe("CashDialog: a sum too large to record", () => {
  it("says it is too large rather than asking for a positive number", () => {
    open();
    typeAmount("20000000000000"); // twice the bound, and positive

    expect(saveButton()).toBeDisabled();
    expect(screen.getByText(/Слишком большая сумма/)).toBeTruthy();
    expect(screen.queryByText(/Введите положительную сумму/)).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("names the largest sum it would take, in the account's currency", () => {
    open();
    typeAmount("10000000000000,01"); // one kopeck past the bound

    // Ten trillion roubles, written the way this screen writes money — not the
    // raw count of kopecks the server speaks in, which would tell the person
    // typing nothing about what to type instead.
    // \s, not a literal space: Intl separates thousands with a non-breaking one
    // and puts a narrow one before the sign, neither of which is the character
    // in this file's source.
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
