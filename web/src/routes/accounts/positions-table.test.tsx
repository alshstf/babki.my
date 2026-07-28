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
const norm = (s: string) => s.replace(/[  ]/g, " ");

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
    ...overrides,
  };
}

describe("PositionsTable", () => {
  it("shows the market value amount, price, and quote date when a quote is available", () => {
    wrap(<PositionsTable positions={[makePosition()]} />);

    expect(norm(screen.getByTestId("position-market-value").textContent ?? "")).toBe(
      norm(formatMinor(305_50, "USD")),
    );
    expect(screen.getByText(/305,50 · 20\.07\.2026/)).toBeInTheDocument();
  });

  it("shows an honest dash with a tooltip instead of a fake zero when there is no quote", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            // Non-zero realized/income/fees so the "no fake 0,00" assertion
            // below can only be satisfied by the market value column
            // behaving — other columns legitimately being 0 would otherwise
            // make that check vacuous.
            realized_pnl_minor: 1_000,
            income_minor: 500,
            fees_minor: 100,
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
});
