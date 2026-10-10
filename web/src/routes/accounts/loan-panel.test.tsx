import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { LoanPanel } from "./loan-panel";
import type { AccountWithBalance } from "@/api/accounts";
import type { Loan } from "@/api/loans";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const base = {
  currency: "RUB",
  institution: "",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  trades_abroad: false,
  kept_by_operations: true,
  counted_by: "journal",
  balance: { as_of: "2026-07-20", amount_minor: 0 },
} as const;

const loanAccount: AccountWithBalance = { ...base, id: "loan-1", name: "Ипотека", type: "loan" };
const card: AccountWithBalance = { ...base, id: "card-1", name: "Карта Сбер", type: "credit_card" };
const dollars: AccountWithBalance = { ...base, id: "usd-1", name: "Доллары", type: "credit_card", currency: "USD" };

// A loan whose next payment is far ahead, so the test does not age.
const loan: Loan = {
  terms: { principal_minor: 120_000_00, annual_rate: "12", term_months: 12, issued_on: "2099-01-01", kind: "annuity" },
  schedule: [
    { on: "2099-02-01", payment_minor: 10_661_85, interest_minor: 1_200_00, principal_minor: 9_461_85, left_minor: 110_538_15, prepaid: false },
    { on: "2099-03-01", payment_minor: 10_661_85, interest_minor: 1_105_38, principal_minor: 9_556_47, left_minor: 100_981_68, prepaid: false },
  ],
  left_by_schedule_minor: 120_000_00,
  total_interest_minor: 7_942_20,
  prepayments: [],
  next: { on: "2099-02-01", payment_minor: 10_661_85, interest_minor: 1_200_00, principal_minor: 9_461_85, left_minor: 110_538_15, prepaid: false },
};

function answer(stated: Loan | null) {
  fetchMock.mockImplementation((input: Request) => {
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
    if (input.url.endsWith("/loan") && input.method === "GET") {
      return stated ? json(stated) : json({ error: "not found" }, 404);
    }
    if (input.url.endsWith("/accounts")) return json([loanAccount, card, dollars]);
    return json(null, input.method === "POST" ? 201 : 200);
  });
}

function show(canEdit = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <LoanPanel account={loanAccount} canEdit={canEdit} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("LoanPanel", () => {
  it("offers to state the terms of a loan that has none", async () => {
    answer(null);
    show();
    expect(await screen.findByText(/Условия кредита не указаны/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Указать условия кредита" }));
    expect(await screen.findByTestId("loan-terms-dialog")).toBeTruthy();
  });

  it("shows the next payment, the debt by the schedule and the schedule itself", async () => {
    answer(loan);
    show();
    const next = await screen.findByTestId("loan-next");
    expect(next.textContent).toMatch(/10\s661,85/);
    expect(screen.getByTestId("loan-left").textContent).toMatch(/120\s000/);
    expect(screen.getAllByTestId("loan-row")).toHaveLength(2);
  });

  it("records the next payment from an account in the loan's currency", async () => {
    answer(loan);
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Внести платёж" }));
    const dialog = await screen.findByTestId("loan-payment-dialog");
    expect(dialog).toBeTruthy();
    // The dollar card is not offered: a payment moves money in one currency.
    await waitFor(() => expect(screen.getByRole("combobox", { name: "С какого счёта" }).textContent).toContain("Карта Сбер"));
    fireEvent.click(screen.getByRole("button", { name: "Записать" }));
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "POST")).toBe(true),
    );
    const post = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "POST")!;
    expect(post.url).toMatch(/\/accounts\/loan-1\/loan\/payments$/);
    expect(await post.json()).toMatchObject({ from_account_id: "card-1", principal_minor: 9_461_85, interest_minor: 1_200_00 });
  });

  it("offers to write the money lent out when the journal holds no debt", async () => {
    answer(loan);
    show();
    const offer = await screen.findByRole("button", { name: /Записать выдачу 120\s000,00\s₽ на 01\.01\.2099/ });
    fireEvent.click(offer);
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "POST")).toBe(true),
    );
    const post = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "POST")!;
    expect(post.url).toMatch(/\/operations$/);
    expect(await post.json()).toMatchObject({
      account_id: "loan-1", type: "withdrawal", occurred_on: "2099-01-01", amount_minor: -120_000_00,
    });
  });

  it("says nothing of the money lent once the journal is in debt", async () => {
    answer(loan);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const inDebt: AccountWithBalance = {
      ...loanAccount,
      journal: {
        amount_minor: -110_000_00, full_amount_minor: -110_000_00, currency: "RUB", unpriced_positions: 0,
        not_traded_positions: 0, missing_rates: [], negative_cash: [], reconciliation: null,
      },
    };
    render(
      <QueryClientProvider client={client}>
        <LoanPanel account={inDebt} canEdit />
      </QueryClientProvider>,
    );
    await screen.findByTestId("loan-next");
    expect(screen.queryByTestId("loan-no-debt")).toBeNull();
  });

  it("pays ahead of the schedule, shortening the term or lowering the payment", async () => {
    answer(loan);
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Погасить досрочно" }));
    await screen.findByTestId("loan-prepayment-dialog");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "С какого счёта" }).textContent).toContain("Карта Сбер"));
    fireEvent.change(screen.getByLabelText(/Сумма, RUB/), { target: { value: "300000" } });
    fireEvent.click(screen.getByRole("button", { name: "Записать" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "POST")).toBe(true));
    const post = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "POST")!;
    expect(post.url).toMatch(/\/loan\/prepayments$/);
    expect(await post.json()).toMatchObject({ from_account_id: "card-1", amount_minor: 300_000_00, mode: "term" });
  });

  it("leaves a viewer only the reading", async () => {
    answer(loan);
    show(false);
    await screen.findByTestId("loan-next");
    expect(screen.queryByRole("button", { name: "Внести платёж" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Изменить условия" })).toBeNull();
  });
});
