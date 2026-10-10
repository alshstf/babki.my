import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import "@/i18n";
import { EverydayBalance } from "./everyday-balance";
import type { AccountWithBalance } from "@/api/accounts";
import type { CashPosition } from "@/api/positions";

afterEach(() => cleanup());

function account(overrides: Partial<AccountWithBalance> = {}): AccountWithBalance {
  return {
    id: "acc-1",
    name: "Карта",
    type: "checking",
    currency: "RUB",
    institution: "",
    status: "active",
    created_at: "2026-01-01T00:00:00Z",
    valued_by_balance: false,
    trades_abroad: false,
    kept_by_operations: false,
    counted_by: "balance",
    balance: { as_of: "2026-10-01", amount_minor: 180_000_00 },
    ...overrides,
  };
}

const cash = (amount: number, overdrawn: string | null = null): CashPosition =>
  ({ currency: "RUB", amount_minor: amount, overdrawn_since: overdrawn, in_base: null }) as unknown as CashPosition;

describe("EverydayBalance", () => {
  it("sets the operations beside the bank's balance and offers to keep by operations", () => {
    const onKeep = vi.fn();
    render(<EverydayBalance account={account()} cash={[cash(179_500_00)]} onKeep={onKeep} />);
    expect(screen.getByTestId("everyday-by-operations")).toHaveTextContent(/179\s500/);
    expect(screen.getByTestId("everyday-by-bank")).toHaveTextContent(/180\s000/);
    expect(screen.getByTestId("everyday-counted-by")).toHaveTextContent("по остатку банка");
    fireEvent.click(screen.getByRole("button", { name: "Вести по операциям" }));
    expect(onKeep).toHaveBeenCalledWith(true);
  });

  it("reads a credit card's minus as a debt, with no alarm", () => {
    render(
      <EverydayBalance
        account={account({
          type: "credit_card",
          counted_by: "journal",
          kept_by_operations: true,
          balance: { as_of: "2026-10-01", amount_minor: -61_500_00 },
          journal: {
            amount_minor: -61_500_00, full_amount_minor: -61_500_00, currency: "RUB", unpriced_positions: 0,
            not_traded_positions: 0, missing_rates: [], negative_cash: ["RUB"],
            reconciliation: { status: "agrees", balance_as_of: "2026-10-01", compared_on: "2026-10-10", balance_in_base_minor: -61_500_00, difference_minor: 0 },
          },
        })}
        cash={[cash(-61_500_00, "2026-08-03")]}
        onOpeningBalance={() => {}}
      />,
    );
    expect(screen.getByText("Долг по операциям")).toBeTruthy();
    expect(screen.getByTestId("everyday-by-operations")).toHaveTextContent(/61\s500/);
    expect(screen.getByTestId("everyday-by-operations").textContent).not.toContain("-");
    expect(screen.getByTestId("everyday-reconciliation")).toHaveTextContent("Сходится");
    expect(screen.queryByText(/ушли в минус/)).toBeNull();
    expect(screen.getByTestId("everyday-counted-by")).toHaveTextContent("по операциям");
  });

  it("offers an opening balance where a card's money went below zero", () => {
    const onOpening = vi.fn();
    render(<EverydayBalance account={account()} cash={[cash(-1_200_00, "2026-09-01")]} onOpeningBalance={onOpening} />);
    fireEvent.click(screen.getByRole("button", { name: "Указать начальный остаток" }));
    expect(onOpening).toHaveBeenCalledWith(expect.objectContaining({ overdrawn_since: "2026-09-01" }));
  });
});
