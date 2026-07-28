import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { PositionsTable } from "./positions-table";
import type { Position } from "@/api/positions";
import { formatMinor } from "@/lib/money";

// PositionsTable is a pure presentational component (positions come in as a
// prop), so a bare render is enough — no QueryClientProvider needed.
function wrap(ui: ReactElement) {
  return render(ui);
}

// NBSP-insensitive compare: Intl.NumberFormat uses non-breaking spaces
// (matches the helper in money.test.ts / summary-cards.test.tsx).
const norm = (s: string) => s.replace(/[  ]/g, " ");

function makePosition(overrides: Partial<Position> = {}): Position {
  return {
    instrument: {
      id: "instr-1",
      type: "share",
      name: "Test Corp",
      ticker: "TEST",
      isin: "US0000000000",
      figi: "",
      currency: "USD",
      frozen: false,
    },
    quantity: "10",
    cost_minor: 250_000,
    realized_pnl_minor: 0,
    income_minor: 0,
    fees_minor: 0,
    currency: "USD",
    market_value_minor: 305_50,
    market_value_currency: "USD",
    price: "305.5",
    price_on: "2026-07-20",
    // 250 000 (2500,00) cost, +250,00 unrealized -> +10,0 %; a plain
    // non-zero default so tests that don't care about the profit column
    // still exercise it rather than accidentally hitting the null branch.
    unrealized_pnl_minor: 25_000,
    ...overrides,
  };
}

describe("PositionsTable", () => {
  it("shows the market value amount and price, with the quote date only in a tooltip", () => {
    wrap(<PositionsTable positions={[makePosition()]} />);

    expect(norm(screen.getByTestId("position-market-value").textContent ?? "")).toBe(
      norm(formatMinor(305_50, "USD")),
    );
    // Price is shown as text...
    const priceLine = screen.getByText("305,50");
    expect(priceLine).toBeInTheDocument();
    // ...but the date is not — it moved into the title tooltip.
    expect(screen.queryByText(/20\.07\.2026/)).not.toBeInTheDocument();
    expect(priceLine).toHaveAttribute("title", "Цена на 20.07.2026");
  });

  it("shows an honest dash with a tooltip instead of a fake zero when there is no quote", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            // Non-zero income so the "no fake 0,00" assertion below can
            // only be satisfied by the market value column behaving —
            // income legitimately being 0 would otherwise make that check
            // vacuous. (Realized/fees are no longer rendered at all.)
            income_minor: 500,
            market_value_minor: null,
            market_value_currency: null,
            price: null,
            price_on: null,
          }),
        ]}
      />,
    );

    const dash = screen.getByTestId("position-no-quote");
    expect(dash).toHaveTextContent("—");
    expect(dash).toHaveAttribute("title", "Нет котировки");
    // "not preceded by a digit" excludes legitimate non-zero amounts that
    // happen to end in "0,00" (e.g. "500,00"), while still catching a real
    // fake-zero amount ("0,00").
    expect(screen.queryByText(/(?<!\d)0,00/)).not.toBeInTheDocument();
    expect(screen.queryByTestId("position-market-value")).not.toBeInTheDocument();
  });

  it("formats the market value by market_value_currency, not the position's own currency", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            currency: "RUB",
            market_value_minor: 100_000,
            market_value_currency: "EUR",
          }),
        ]}
      />,
    );

    const amount = screen.getByTestId("position-market-value");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(100_000, "EUR")));
    expect(amount.textContent).not.toMatch(/₽/);
  });

  it("does not render the removed realized/fees columns", () => {
    wrap(<PositionsTable positions={[makePosition()]} />);

    expect(screen.queryByText("Реализовано")).not.toBeInTheDocument();
    expect(screen.queryByText("Комиссии")).not.toBeInTheDocument();
    expect(screen.getByText("Прибыль")).toBeInTheDocument();
  });

  it("shows unrealized profit with its percentage of cost", () => {
    // cost 2500,00, unrealized +250,00 -> +10,0 %
    wrap(<PositionsTable positions={[makePosition({ cost_minor: 250_000, unrealized_pnl_minor: 25_000 })]} />);

    const amount = screen.getByTestId("position-profit-amount");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(25_000, "USD")));
    expect(amount.className).toContain("text-emerald-500");
    expect(norm(screen.getByTestId("position-profit-percent").textContent ?? "")).toBe(
      norm("+10,0 %"),
    );
  });

  it("shows unrealized loss in red with a negative percentage", () => {
    // cost 2500,00, unrealized -300,00 -> -12,0 %
    wrap(
      <PositionsTable
        positions={[makePosition({ cost_minor: 250_000, unrealized_pnl_minor: -30_000 })]}
      />,
    );

    const amount = screen.getByTestId("position-profit-amount");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(-30_000, "USD")));
    expect(amount.className).toContain("text-red-500");
    expect(norm(screen.getByTestId("position-profit-percent").textContent ?? "")).toBe(
      norm("-12,0 %"),
    );
  });

  it("shows a dash with a tooltip for the profit column when unrealized_pnl_minor is null", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            market_value_minor: null,
            market_value_currency: null,
            unrealized_pnl_minor: null,
          }),
        ]}
      />,
    );

    const dash = screen.getByTestId("position-profit-dash");
    expect(dash).toHaveTextContent("—");
    expect(dash).toHaveAttribute("title", "Нет котировки");
    expect(screen.queryByTestId("position-profit-amount")).not.toBeInTheDocument();
    expect(screen.queryByTestId("position-profit-percent")).not.toBeInTheDocument();
  });

  it("shows a dash with currency mismatch tooltip when unrealized_pnl_minor is null but market value exists in different currency", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            currency: "RUB",
            market_value_minor: 952_00,
            market_value_currency: "USD",
            unrealized_pnl_minor: null,
          }),
        ]}
      />,
    );

    const marketValue = screen.getByTestId("position-market-value");
    expect(norm(marketValue.textContent ?? "")).toBe(norm(formatMinor(952_00, "USD")));

    const dash = screen.getByTestId("position-profit-dash");
    expect(dash).toHaveTextContent("—");
    expect(dash).toHaveAttribute("title", "Оценка в другой валюте — прибыль не рассчитывается");
    expect(screen.queryByTestId("position-profit-amount")).not.toBeInTheDocument();
    expect(screen.queryByTestId("position-profit-percent")).not.toBeInTheDocument();
  });

  it("omits the percentage (but still shows the amount) when cost is 0", () => {
    wrap(
      <PositionsTable
        positions={[makePosition({ cost_minor: 0, unrealized_pnl_minor: 1_000 })]}
      />,
    );

    const amount = screen.getByTestId("position-profit-amount");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(1_000, "USD")));
    expect(screen.queryByTestId("position-profit-percent")).not.toBeInTheDocument();
  });
});
