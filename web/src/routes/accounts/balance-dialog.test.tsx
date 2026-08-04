import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { BalanceDialog } from "./balance-dialog";
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
      <BalanceDialog open onOpenChange={() => {}} account={account} />
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

// #89: this field was the one door into a balance, and it bounded nothing. What
// it sent, the server took; what the server took, the accounts screen then could
// not convert, answering 500 for every account in the space for as long as the
// row existed. The server refuses now, which is the check that matters — these
// are about the field refusing at the keystroke, so nobody learns it from a red
// box, and about the field saying WHICH of the two things is wrong.
describe("BalanceDialog: a sum too large to record", () => {
  it("does not send it, and says it is too large rather than unreadable", () => {
    open();
    typeAmount("10000000000000,01"); // one kopeck past the bound

    expect(saveButton()).toBeDisabled();
    // The number parses perfectly well, so the parse error would be a caption
    // naming a cause that is not the cause.
    expect(screen.queryByText(/Не удалось разобрать сумму/)).toBeNull();
    expect(screen.getByText(/Слишком большая сумма/)).toBeTruthy();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("names the largest sum it would take, in the account's currency", () => {
    open();
    typeAmount("10000000000000,01");

    // Ten trillion roubles, written the way this screen writes money — not the
    // raw count of kopecks the server speaks in, which would tell the person
    // typing nothing about what to type instead.
    // \s, not a literal space: Intl separates thousands with a non-breaking one
    // and puts a narrow one before the sign, neither of which is the character
    // in this file's source.
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
