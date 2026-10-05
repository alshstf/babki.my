import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { IncomeDialog } from "./income-dialog";
import type { AccountWithBalance } from "@/api/accounts";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// The dialog's InstrumentPicker always queries the catalog; a fresh
// Response per call, since a body can be read only once.
function serveEmptyCatalog() {
  fetchMock.mockImplementation(() =>
    Promise.resolve(
      new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } }),
    ),
  );
}

const account: AccountWithBalance = {
  id: "acc-1",
  name: "Брокерский",
  type: "brokerage",
  currency: "RUB",
  institution: "Broker Co",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  counted_by: "balance",
  balance: { as_of: "2026-07-20", amount_minor: 1_000_000 },
};

function open() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <IncomeDialog open onOpenChange={() => {}} account={account} />
    </QueryClientProvider>,
  );
}

const amountField = () => screen.getByLabelText(/Сумма/);
const saveButton = () => screen.getByRole("button", { name: "Сохранить" });

function typeAmount(value: string) {
  fireEvent.change(amountField(), { target: { value } });
}

beforeEach(() => {
  fetchMock.mockReset();
  serveEmptyCatalog();
});

afterEach(() => {
  cleanup();
});

// A payout past the bound gets «Слишком большая сумма», not «Введите
// положительную сумму», which is false of a positive, parseable number.
describe("IncomeDialog: a sum too large to record", () => {
  it("says it is too large rather than asking for a positive number", () => {
    open();
    typeAmount("20000000000000");

    expect(saveButton()).toBeDisabled();
    expect(screen.getByText(/Слишком большая сумма/)).toBeTruthy();
    expect(screen.queryByText(/Введите положительную сумму/)).toBeNull();
  });

  it("names the largest sum it would take, in the account's currency", () => {
    open();
    typeAmount("10000000000000,01"); // one kopeck past the bound

    // \s, not a space: Intl uses non-breaking and narrow spaces.
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

  it("takes an ordinary dividend", () => {
    open();
    typeAmount("1 250,40");

    expect(saveButton()).not.toBeDisabled();
    expect(screen.queryByText(/Слишком большая сумма/)).toBeNull();
  });
});

// #109.2: dividend and coupon add to income, but the engine records an
// amortization as a disposal (TypeAmortization in
// internal/portfolio/engine.go), so it never reaches the «Доход» column.
const AMORTIZATION_NOTE =
  "Амортизация — это возврат части номинала. Программа записывает её как выбытие, а не как доход: в колонке «Доход» она не появится";

describe("IncomeDialog: an amortization is not income", () => {
  const typeSelect = () => screen.getByRole("combobox");

  function chooseType(label: string) {
    // jsdom lacks scrollIntoView, which Radix Select calls on open.
    Element.prototype.scrollIntoView = () => {};
    fireEvent.click(typeSelect());
    fireEvent.click(screen.getByText(label));
  }

  it("does not call the whole form income", () => {
    open();

    // «Выплата» is true of all three; «Доход» was true of two.
    expect(screen.getByText("Выплата по инструменту")).toBeInTheDocument();
    expect(screen.queryByText("Доход по инструменту")).toBeNull();
  });

  it("says where an amortization actually lands, once one is chosen", () => {
    open();
    chooseType("амортизация");

    expect(screen.getByTestId("income-amortization-note").textContent).toBe(AMORTIZATION_NOTE);
  });

  it("says nothing of the sort for a dividend, which is income", () => {
    open();

    expect(screen.queryByTestId("income-amortization-note")).toBeNull();
  });

  it("takes the note away again when the type moves off an amortization", () => {
    open();
    chooseType("амортизация");
    expect(screen.getByTestId("income-amortization-note")).toBeInTheDocument();

    chooseType("купон");

    expect(screen.queryByTestId("income-amortization-note")).toBeNull();
  });
});
