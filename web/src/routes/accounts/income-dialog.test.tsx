import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { IncomeDialog } from "./income-dialog";
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

// An empty catalog, served fresh on every call: this dialog mounts an
// InstrumentPicker, which queries the catalog whether or not the test is about
// instruments, and a single Response handed to mockResolvedValue would work
// once and then throw, because a body can only be consumed once.
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

// A dividend past the bound is a dividend nobody will ever be paid, and this
// field takes it exactly as seriously as the deposit field beside it does —
// same number, same screen, same currency. What is pinned here is WHICH
// sentence it gets: «Введите положительную сумму» would be false of
// 20 000 000 000 000, which is positive and parses, and a caption naming a
// cause that is not the cause is the mistake this repository keeps rediscovering.
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

  it("takes an ordinary dividend", () => {
    open();
    typeAmount("1 250,40");

    expect(saveButton()).not.toBeDisabled();
    expect(screen.queryByText(/Слишком большая сумма/)).toBeNull();
  });
});

// #109.2. This form records three operation types and the engine treats them
// as two different things: dividend and coupon add to Position.IncomeByCurrency,
// while amortization is written as a DISPOSAL — p.realize(...) with the
// returned principal as proceeds, the retired basis as its released pieces,
// and the income untouched (the TypeAmortization branch in
// internal/portfolio/engine.go). So an amortization never appears in the
// «Доход» column of the positions table, no matter how large it is, and a
// form that called it «Доход по инструменту» promised a figure that is not
// coming.
const AMORTIZATION_NOTE =
  "Амортизация — это возврат части номинала. Программа записывает её как выбытие, а не как доход: в колонке «Доход» она не появится";

describe("IncomeDialog: an amortization is not income", () => {
  const typeSelect = () => screen.getByRole("combobox");

  function chooseType(label: string) {
    // jsdom implements no layout, so Element.prototype.scrollIntoView simply
    // does not exist — and Radix's Select calls it on the selected item when
    // the list opens. The stub fills a hole in the environment, not in the
    // component: nothing below asserts anything about scrolling, and without
    // it the list cannot be opened at all.
    Element.prototype.scrollIntoView = () => {};
    fireEvent.click(typeSelect());
    fireEvent.click(screen.getByText(label));
  }

  it("does not call the whole form income", () => {
    open();

    // «Выплата» is true of all three: a dividend, a coupon and a return of
    // principal are all money the issuer pays out. «Доход» was true of two.
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
