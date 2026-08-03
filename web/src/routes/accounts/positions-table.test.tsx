import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
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
// (matches the helper in money.test.ts / summary-cards.test.tsx). Written
// with explicit escapes (rather than the literal characters) so they can't
// silently get mangled into plain ASCII spaces by an editing tool.
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

// The four sentences the row's caption can be, one per value of the server's
// Position.in_base_gap, plus the general one a client that cannot name the
// server's answer falls back to. Spelled out here in full rather than read
// back out of ru.json: the whole point of #66 is WHICH sentence a cell gets,
// and a test that fetched the sentence through the same lookup the component
// uses would agree with the component no matter which one it picked.
const CAPTION = {
  general: "\u041D\u0435\u0442 \u043A\u0443\u0440\u0441\u0430 \u2014 \u043F\u043E\u043A\u0430\u0437\u0430\u043D\u043E \u0432 \u0438\u0441\u0445\u043E\u0434\u043D\u043E\u0439 \u0432\u0430\u043B\u044E\u0442\u0435",
  // #66's wording fix: the old ending (\u00AB\u0441\u0442\u0440\u043E\u043A\u0430 \u043F\u043E\u043A\u0430\u0437\u044B\u0432\u0430\u0435\u0442\u0441\u044F \u0432 \u0431\u0430\u0437\u043E\u0432\u043E\u0439 \u0432\u0430\u043B\u044E\u0442\u0435
  // \u0446\u0435\u043B\u0438\u043A\u043E\u043C \u0438\u043B\u0438 \u043D\u0438\u043A\u0430\u043A\u00BB) stated a rule about how EVERY row displays, and a row
  // with in_base present plus its own market_value_gap breaks it (cost/income
  // in rubles, valuation in euros, on the same row). The new ending is scoped
  // to the row this caption sits on and stays true regardless of what a
  // different row is doing.
  undatedLot:
    "\u0423 \u043E\u0434\u043D\u043E\u0439 \u0438\u0437 \u043F\u0430\u0440\u0442\u0438\u0439 \u043D\u0435 \u0437\u0430\u043F\u0438\u0441\u0430\u043D\u0430 \u0434\u0430\u0442\u0430 \u043F\u043E\u043A\u0443\u043F\u043A\u0438, \u0430 \u0441\u0442\u043E\u0438\u043C\u043E\u0441\u0442\u044C \u0441\u0447\u0438\u0442\u0430\u0435\u0442\u0441\u044F \u043F\u043E \u043A\u0443\u0440\u0441\u0443 \u043D\u0430 \u0434\u0435\u043D\u044C \u043F\u043E\u043A\u0443\u043F\u043A\u0438 \u2014 \u0438 \u0432\u043E\u0441\u0441\u0442\u0430\u043D\u043E\u0432\u0438\u0442\u044C \u044D\u0442\u0443 \u0434\u0430\u0442\u0443 \u0443\u0436\u0435 \u043D\u0435\u043E\u0442\u043A\u0443\u0434\u0430: \u0432 \u0431\u0430\u0437\u043E\u0432\u043E\u0439 \u0432\u0430\u043B\u044E\u0442\u0435 \u044D\u0442\u0430 \u043F\u043E\u0437\u0438\u0446\u0438\u044F \u0441\u0430\u043C\u0430 \u043D\u0435 \u043F\u043E\u0441\u0447\u0438\u0442\u0430\u0435\u0442\u0441\u044F. \u041F\u043E\u044D\u0442\u043E\u043C\u0443 \u043D\u0438 \u043E\u0434\u043D\u043E \u0447\u0438\u0441\u043B\u043E \u044D\u0442\u043E\u0439 \u0441\u0442\u0440\u043E\u043A\u0438 \u043D\u0435 \u043F\u043E\u043A\u0430\u0437\u0430\u043D\u043E \u0432 \u0431\u0430\u0437\u043E\u0432\u043E\u0439 \u0432\u0430\u043B\u044E\u0442\u0435",
  noRateLotDate:
    "\u041D\u0435\u0442 \u043A\u0443\u0440\u0441\u0430 \u043D\u0430 \u0434\u0435\u043D\u044C \u043F\u043E\u043A\u0443\u043F\u043A\u0438 \u043E\u0434\u043D\u043E\u0439 \u0438\u0437 \u043F\u0430\u0440\u0442\u0438\u0439, \u0430 \u0441\u0442\u043E\u0438\u043C\u043E\u0441\u0442\u044C \u0441\u0447\u0438\u0442\u0430\u0435\u0442\u0441\u044F \u043F\u043E \u043A\u0443\u0440\u0441\u0443 \u0442\u043E\u0433\u043E \u0434\u043D\u044F. \u041A\u0443\u0440\u0441 \u043F\u043E\u044F\u0432\u0438\u0442\u0441\u044F \u043F\u0440\u0438 \u043E\u0431\u043D\u043E\u0432\u043B\u0435\u043D\u0438\u0438 \u043A\u0443\u0440\u0441\u043E\u0432, \u0438 \u043F\u043E\u0437\u0438\u0446\u0438\u044F \u043F\u043E\u0441\u0447\u0438\u0442\u0430\u0435\u0442\u0441\u044F \u0441\u0430\u043C\u0430. \u041F\u043E\u044D\u0442\u043E\u043C\u0443 \u043F\u043E\u043A\u0430 \u043D\u0438 \u043E\u0434\u043D\u043E \u0447\u0438\u0441\u043B\u043E \u044D\u0442\u043E\u0439 \u0441\u0442\u0440\u043E\u043A\u0438 \u043D\u0435 \u043F\u043E\u043A\u0430\u0437\u0430\u043D\u043E \u0432 \u0431\u0430\u0437\u043E\u0432\u043E\u0439 \u0432\u0430\u043B\u044E\u0442\u0435",
  noRateIncomeDate:
    "\u041A\u0443\u0440\u0441\u044B \u043D\u0430 \u0434\u043D\u0438 \u043F\u043E\u043A\u0443\u043F\u043E\u043A \u043D\u0430\u0448\u043B\u0438\u0441\u044C, \u0430 \u043D\u0430 \u0434\u0435\u043D\u044C \u043E\u0434\u043D\u043E\u0439 \u0438\u0437 \u0432\u044B\u043F\u043B\u0430\u0442 \u2014 \u0434\u0438\u0432\u0438\u0434\u0435\u043D\u0434\u0430, \u043A\u0443\u043F\u043E\u043D\u0430 \u0438\u043B\u0438 \u043D\u0430\u043B\u043E\u0433\u0430 \u2014 \u043D\u0435\u0442; \u0434\u043E\u0445\u043E\u0434 \u0441\u0447\u0438\u0442\u0430\u0435\u0442\u0441\u044F \u043F\u043E \u043A\u0443\u0440\u0441\u0443 \u0442\u043E\u0433\u043E \u0434\u043D\u044F. \u041A\u0443\u0440\u0441 \u043F\u043E\u044F\u0432\u0438\u0442\u0441\u044F \u043F\u0440\u0438 \u043E\u0431\u043D\u043E\u0432\u043B\u0435\u043D\u0438\u0438 \u043A\u0443\u0440\u0441\u043E\u0432, \u0438 \u043F\u043E\u0437\u0438\u0446\u0438\u044F \u043F\u043E\u0441\u0447\u0438\u0442\u0430\u0435\u0442\u0441\u044F \u0441\u0430\u043C\u0430. \u041F\u043E\u044D\u0442\u043E\u043C\u0443 \u043F\u043E\u043A\u0430 \u043D\u0438 \u043E\u0434\u043D\u043E \u0447\u0438\u0441\u043B\u043E \u044D\u0442\u043E\u0439 \u0441\u0442\u0440\u043E\u043A\u0438 \u043D\u0435 \u043F\u043E\u043A\u0430\u0437\u0430\u043D\u043E \u0432 \u0431\u0430\u0437\u043E\u0432\u043E\u0439 \u0432\u0430\u043B\u044E\u0442\u0435",
  noRateToday:
    "\u041A\u0443\u0440\u0441\u044B \u043D\u0430 \u0434\u043D\u0438 \u043F\u043E\u043A\u0443\u043F\u043E\u043A \u0438 \u0432\u044B\u043F\u043B\u0430\u0442 \u043D\u0430\u0448\u043B\u0438\u0441\u044C, \u0430 \u043D\u0430 \u0441\u0435\u0433\u043E\u0434\u043D\u044F \u2014 \u0434\u043B\u044F \u0432\u0430\u043B\u044E\u0442\u044B, \u0432 \u043A\u043E\u0442\u043E\u0440\u043E\u0439 \u0441\u0447\u0438\u0442\u0430\u0435\u0442\u0441\u044F \u0440\u044B\u043D\u043E\u0447\u043D\u0430\u044F \u043E\u0446\u0435\u043D\u043A\u0430, \u2014 \u043D\u0435\u0442; \u043E\u0446\u0435\u043D\u043A\u0430 \u0431\u0435\u0440\u0451\u0442\u0441\u044F \u043F\u043E \u0442\u0435\u043A\u0443\u0449\u0435\u043C\u0443 \u043A\u0443\u0440\u0441\u0443. \u041A\u0443\u0440\u0441 \u043F\u043E\u044F\u0432\u0438\u0442\u0441\u044F \u043F\u0440\u0438 \u043E\u0431\u043D\u043E\u0432\u043B\u0435\u043D\u0438\u0438 \u043A\u0443\u0440\u0441\u043E\u0432, \u0438 \u043F\u043E\u0437\u0438\u0446\u0438\u044F \u043F\u043E\u0441\u0447\u0438\u0442\u0430\u0435\u0442\u0441\u044F \u0441\u0430\u043C\u0430. \u041F\u043E\u044D\u0442\u043E\u043C\u0443 \u043F\u043E\u043A\u0430 \u043D\u0438 \u043E\u0434\u043D\u043E \u0447\u0438\u0441\u043B\u043E \u044D\u0442\u043E\u0439 \u0441\u0442\u0440\u043E\u043A\u0438 \u043D\u0435 \u043F\u043E\u043A\u0430\u0437\u0430\u043D\u043E \u0432 \u0431\u0430\u0437\u043E\u0432\u043E\u0439 \u0432\u0430\u043B\u044E\u0442\u0435",
  valuationCurrency:
    "\u041E\u0446\u0435\u043D\u043A\u0430 \u043F\u043E\u043B\u0443\u0447\u0438\u043B\u0430\u0441\u044C \u0432 \u0442\u0440\u0435\u0442\u044C\u0435\u0439 \u0432\u0430\u043B\u044E\u0442\u0435 \u2014 \u043D\u0435 \u0432 \u0432\u0430\u043B\u044E\u0442\u0435 \u043F\u043E\u0437\u0438\u0446\u0438\u0438 \u0438 \u043D\u0435 \u0432 \u0431\u0430\u0437\u043E\u0432\u043E\u0439, \u2014 \u0430 \u043A\u0443\u0440\u0441\u0430 \u043E\u0442 \u043D\u0435\u0451 \u0434\u043E \u0432\u0430\u043B\u044E\u0442\u044B \u043F\u043E\u0437\u0438\u0446\u0438\u0438 \u043D\u0435\u0442: \u0441\u0440\u0430\u0432\u043D\u0438\u0442\u044C \u0435\u0451 \u0441\u043E \u0441\u0442\u043E\u0438\u043C\u043E\u0441\u0442\u044C\u044E \u043F\u043E\u0437\u0438\u0446\u0438\u0438 \u043D\u0435\u043B\u044C\u0437\u044F. \u041F\u043E\u043A\u0430 \u043E\u0446\u0435\u043D\u043A\u0430 \u043D\u0435 \u0432\u044B\u0440\u0430\u0436\u0435\u043D\u0430 \u0432 \u0432\u0430\u043B\u044E\u0442\u0435 \u043F\u043E\u0437\u0438\u0446\u0438\u0438, \u043F\u0440\u043E\u0433\u0440\u0430\u043C\u043C\u0430 \u043D\u0435 \u043F\u043E\u043A\u0430\u0437\u044B\u0432\u0430\u0435\u0442 \u0435\u0451 \u0438 \u0432 \u0431\u0430\u0437\u043E\u0432\u043E\u0439. \u041F\u043E\u044D\u0442\u043E\u043C\u0443 \u043F\u043E\u043A\u0430\u0437\u0430\u043D\u0430 \u0432 \u0438\u0441\u0445\u043E\u0434\u043D\u043E\u0439 \u0432\u0430\u043B\u044E\u0442\u0435",
} as const;

// Every money cell of a row carries the row's caption; the valuation is the
// one that can carry its own instead (market_value_gap). Named here so a test
// can say "all four" without repeating the ids.
const ROW_MARKERS = [
  "position-cost-not-converted",
  "position-market-value-not-converted",
  "position-profit-amount-not-converted",
  "position-income-not-converted",
] as const;

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
    // The ordinary case: every lot came from a buy, and a buy always knows
    // its own date. Tests about the other case set it explicitly.
    has_undated_lots: false,
    // Same default for the realized-side twin: no test in this file exercises
    // it yet, because PositionsTable does not read it — nothing in this
    // component surfaces a per-position realized-undated indicator, so the
    // field stays at its honest default rather than an untested guess.
    has_undated_realizations: false,
    // The two server-named causes (#66), and the only source the row's and the
    // valuation's captions read. Null is the honest default for this fixture:
    // it converts cleanly, so nothing stopped the object and nothing was
    // withheld from the valuation. A test that nulls in_base must set
    // in_base_gap too — the server publishes the two together (see
    // Position.in_base_gap in the API contract: non-null exactly when in_base
    // is null and the currency differs from the base one).
    in_base_gap: null,
    market_value_gap: null,
    ...overrides,
  };
}

describe("PositionsTable", () => {
  it("shows the market value amount and price, with the quote date only in a tooltip", () => {
    wrap(<PositionsTable positions={[makePosition()]} mode="native" baseCurrency="RUB" />);

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
        mode="native"
        baseCurrency="RUB"
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
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const amount = screen.getByTestId("position-market-value");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(100_000, "EUR")));
    expect(amount.textContent).not.toMatch(/₽/);
  });

  it("does not render the removed realized/fees columns", () => {
    wrap(<PositionsTable positions={[makePosition()]} mode="native" baseCurrency="RUB" />);

    expect(screen.queryByText("Реализовано")).not.toBeInTheDocument();
    expect(screen.queryByText("Комиссии")).not.toBeInTheDocument();
    expect(screen.getByText("Прибыль")).toBeInTheDocument();
  });

  it("shows unrealized profit with its percentage of cost", () => {
    // cost 2500,00, unrealized +250,00 -> +10,0 %
    wrap(
      <PositionsTable
        positions={[makePosition({ cost_minor: 250_000, unrealized_pnl_minor: 25_000 })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const amount = screen.getByTestId("position-profit-amount");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(25_000, "USD")));
    expect(amount.className).toContain("text-emerald-500");
    expect(norm(screen.getByTestId("position-profit-percent").textContent ?? "")).toBe(
      norm("+10,0 %"),
    );
  });

  it("shows unrealized loss in red with a negative percentage", () => {
    // cost 2500,00, unrealized -300,00 -> -12,0 %
    wrap(
      <PositionsTable
        positions={[makePosition({ cost_minor: 250_000, unrealized_pnl_minor: -30_000 })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const amount = screen.getByTestId("position-profit-amount");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(-30_000, "USD")));
    expect(amount.className).toContain("text-red-500");
    expect(norm(screen.getByTestId("position-profit-percent").textContent ?? "")).toBe(
      norm("-12,0 %"),
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
        mode="native"
        baseCurrency="RUB"
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
        mode="native"
        baseCurrency="RUB"
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

  it("adds a tooltip note with the pre-conversion amount when the market value was converted from another currency", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            currency: "USD",
            market_value_minor: 305_50,
            market_value_currency: "USD",
            market_value_source_currency: "RUB",
            market_value_source_minor: 2_800_00,
            unrealized_pnl_minor: 25_000,
          }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    // The converted (position-currency) amount is still what's shown in the DOM...
    const amount = screen.getByTestId("position-market-value");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(305_50, "USD")));
    // ...profit is computed normally (not the currency-mismatch dash)...
    const profitAmount = screen.getByTestId("position-profit-amount");
    expect(norm(profitAmount.textContent ?? "")).toBe(norm(formatMinor(25_000, "USD")));
    // ...and the source amount appears only in the tooltip, never as text.
    const sourceAmount = formatMinor(2_800_00, "RUB");
    expect(screen.queryByText(sourceAmount)).not.toBeInTheDocument();
    const priceLine = screen.getByText("305,50");
    expect(norm(priceLine.getAttribute("title") ?? "")).toBe(
      norm(`Цена на 20.07.2026\nПересчитано из ${sourceAmount}`),
    );
  });

  it("omits the converted-from tooltip line when the market value has no source currency/amount", () => {
    wrap(<PositionsTable positions={[makePosition()]} mode="native" baseCurrency="RUB" />);

    const priceLine = screen.getByText("305,50");
    expect(priceLine.getAttribute("title")).toBe("Цена на 20.07.2026");
  });

  // A bond is quoted as a PERCENTAGE of its face value (95.20 meaning 95.20 %
  // of the face value), and that percentage is what the server publishes in
  // Position.price — see marketValue() in internal/portfolio/http.go, where a
  // bond's valuation is faceValueMinor × price/100 × quantity. The demo seed's
  // OFZ26238 is exactly this: face value 1 000,00 ₽, quote 95.20, so the money
  // one bond is worth is 952 ₽ and the figure under the valuation is 95,20.
  // Printed bare beside a ruble amount that reading is off by a factor of ten
  // (#32), so the unit is stated. What is NOT done is deriving the 952 ₽:
  // that is money arithmetic in the browser, which this project does not do.
  const BOND_PRICE_NOTE =
    "Облигация котируется в процентах от номинала, а не в деньгах за штуку: рыночная оценка выше — это номинал, умноженный на этот процент и на количество";

  function makeBond(overrides: Partial<Position> = {}): Position {
    return makePosition({
      instrument: {
        id: "instr-bond",
        type: "bond",
        name: "ОФЗ 26238",
        ticker: "OFZ26238",
        isin: "RU000A1038V6",
        figi: "",
        currency: "RUB",
        face_value_minor: 100_000,
        face_currency: "RUB",
        frozen: false,
      },
      currency: "RUB",
      // 100 bonds × 1 000,00 ₽ face × 95.20 % = 95 200,00 ₽ — the number the
      // valuation cell shows, which the price below it is NOT a per-unit slice of.
      quantity: "100",
      cost_minor: 9_000_000,
      market_value_minor: 9_520_000,
      market_value_currency: "RUB",
      unrealized_pnl_minor: 520_000,
      price: "95.20",
      price_on: "2026-07-20",
      ...overrides,
    });
  }

  it("marks a bond's price as a percentage of face value and says so in the tooltip", () => {
    wrap(<PositionsTable positions={[makeBond()]} mode="native" baseCurrency="RUB" />);

    const priceLine = screen.getByTestId("position-price");
    expect(norm(priceLine.textContent ?? "")).toBe("95,20 %");
    expect(norm(priceLine.getAttribute("title") ?? "")).toBe(
      norm(`Цена на 20.07.2026\n${BOND_PRICE_NOTE}`),
    );
  });

  it.each([["share"], ["etf"]] as const)(
    "leaves a %s's price bare — its quote really is money per unit",
    (type) => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              instrument: { ...makePosition().instrument, type },
            }),
          ]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      const priceLine = screen.getByTestId("position-price");
      expect(norm(priceLine.textContent ?? "")).toBe("305,50");
      expect(priceLine.textContent).not.toContain("%");
      expect(priceLine.getAttribute("title")).toBe("Цена на 20.07.2026");
      expect(priceLine.getAttribute("title")).not.toContain(BOND_PRICE_NOTE);
    },
  );

  it("keeps the converted-from line alongside the percentage note on a bond", () => {
    const sourceAmount = formatMinor(9_520_000, "RUB");
    wrap(
      <PositionsTable
        positions={[
          makeBond({
            currency: "USD",
            market_value_minor: 105_777,
            market_value_currency: "USD",
            market_value_source_currency: "RUB",
            market_value_source_minor: 9_520_000,
          }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const priceLine = screen.getByTestId("position-price");
    expect(norm(priceLine.textContent ?? "")).toBe("95,20 %");
    expect(norm(priceLine.getAttribute("title") ?? "")).toBe(
      norm(`Цена на 20.07.2026\n${BOND_PRICE_NOTE}\nПересчитано из ${sourceAmount}`),
    );
  });

  it("omits the percentage (but still shows the amount) when cost is 0", () => {
    wrap(
      <PositionsTable
        positions={[makePosition({ cost_minor: 0, unrealized_pnl_minor: 1_000 })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const amount = screen.getByTestId("position-profit-amount");
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinor(1_000, "USD")));
    expect(screen.queryByTestId("position-profit-percent")).not.toBeInTheDocument();
  });

  describe("base mode", () => {
    it("shows cost, market value, profit and income all converted into the base currency when in_base is present", () => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              cost_minor: 250_000,
              income_minor: 1_000,
              in_base: {
                cost_minor: 2_275_000,
                market_value_minor: 2_780_050,
                unrealized_pnl_minor: 227_500,
                income_minor: 9_100,
                currency: "RUB",
                rate_on: "2026-07-20",
              },
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      expect(norm(screen.getByTestId("position-cost").textContent ?? "")).toBe(
        norm(formatMinor(2_275_000, "RUB")),
      );
      expect(norm(screen.getByTestId("position-market-value").textContent ?? "")).toBe(
        norm(formatMinor(2_780_050, "RUB")),
      );
      expect(norm(screen.getByTestId("position-profit-amount").textContent ?? "")).toBe(
        norm(formatMinor(227_500, "RUB")),
      );
      expect(norm(screen.getByTestId("position-income").textContent ?? "")).toBe(
        norm(formatMinor(9_100, "RUB")),
      );
      // No "not converted" indicators anywhere — every figure had a rate.
      // (Checked by test id, not by text: the marker is an icon whose
      // wording lives in a title attribute, so a text query can never see it
      // and would pass vacuously.)
      for (const testId of [
        "position-cost",
        "position-market-value",
        "position-profit-amount",
        "position-income",
      ]) {
        expect(screen.queryByTestId(`${testId}-not-converted`)).not.toBeInTheDocument();
      }
    });

    it("computes the profit percentage from the converted cost/profit pair, not the native one", () => {
      // Native: cost 2500,00 / profit 250,00 -> +10,0 %. This fixture is
      // deliberately built so both figures scale by the same factor (8), so
      // the two currencies agree on +10,0 % and the ONLY way to get a wrong
      // percentage is to mix a native number with a converted one (e.g.
      // native cost against converted profit). Real data does not usually
      // agree like this — the backend's cost is struck at each lot's own
      // historical rate while the profit carries today's valuation, so the
      // two percentages normally differ (the test below pins exactly that).
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              cost_minor: 250_000,
              unrealized_pnl_minor: 25_000,
              in_base: {
                cost_minor: 2_000_000,
                market_value_minor: 2_200_000,
                unrealized_pnl_minor: 200_000,
                income_minor: 0,
                currency: "RUB",
                rate_on: "2026-07-20",
              },
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      expect(norm(screen.getByTestId("position-profit-percent").textContent ?? "")).toBe(
        norm("+10,0 %"),
      );
    });

    it("omits the percentage when profit stays native while cost converts", () => {
      // in_base carries a null unrealized_pnl_minor whenever the valuation
      // could not be expressed in the position's own currency, so cost
      // resolves to RUB while profit stays in USD. Dividing 250,00 $ by
      // 225 000,00 ₽ would print +0,1 % for a position that actually gained
      // +10 %: a wrong number is worse than no number.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              cost_minor: 250_000,
              unrealized_pnl_minor: 25_000,
              in_base: {
                cost_minor: 22_500_000,
                market_value_minor: null,
                unrealized_pnl_minor: null,
                income_minor: 0,
                currency: "RUB",
                rate_on: "2026-07-20",
              },
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      // The amount itself is still shown, honestly, in its own currency.
      expect(norm(screen.getByTestId("position-profit-amount").textContent ?? "")).toBe(
        norm(formatMinor(25_000, "USD")),
      );
      expect(screen.getByTestId("position-profit-amount-not-converted")).toBeInTheDocument();
      expect(screen.queryByTestId("position-profit-percent")).not.toBeInTheDocument();
    });

    it("shows the native amounts plus a not-converted indicator on every money cell when in_base is null and the currency differs from base", () => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              cost_minor: 250_000,
              income_minor: 1_000,
              unrealized_pnl_minor: 25_000,
              in_base: null,
              in_base_gap: "no_rate_lot_date",
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      // Honest native fallback everywhere — no dash, no fabricated zero.
      expect(norm(screen.getByTestId("position-cost").textContent ?? "")).toContain(
        norm(formatMinor(250_000, "USD")),
      );
      expect(norm(screen.getByTestId("position-market-value").textContent ?? "")).toContain(
        norm(formatMinor(305_50, "USD")),
      );
      expect(norm(screen.getByTestId("position-profit-amount").textContent ?? "")).toContain(
        norm(formatMinor(25_000, "USD")),
      );
      expect(norm(screen.getByTestId("position-income").textContent ?? "")).toContain(
        norm(formatMinor(1_000, "USD")),
      );

      expect(screen.getByTestId("position-cost-not-converted")).toHaveAttribute(
        "title",
        CAPTION.noRateLotDate,
      );
      expect(screen.getByTestId("position-market-value-not-converted")).toBeInTheDocument();
      expect(screen.getByTestId("position-profit-amount-not-converted")).toBeInTheDocument();
      expect(screen.getByTestId("position-income-not-converted")).toBeInTheDocument();
    });

    // #66. Four different things stop in_base, the server names the one it
    // stopped on, and a reader told «нет курса» over all four is told something
    // false about three of them. Each case below pins its OWN sentence on every
    // cell of the row and pins that it is not any of the other three — a test
    // that only asked "is there a caption" would pass with the causes swapped,
    // which is the exact defect this branch exists to fix.
    describe("the caption names the term the server stopped on", () => {
      // Renders one position that failed for `gap` and returns the titles of
      // the four markers, so each case can assert on all of them at once.
      // Unmounts whatever a previous call rendered first: a case that asks for
      // several gaps in a row would otherwise leave two tables in one document
      // and every getByTestId below would find two elements.
      function captionsFor(gap: NonNullable<Position["in_base_gap"]>): string[] {
        cleanup();
        wrap(
          <PositionsTable
            positions={[
              makePosition({
                currency: "USD",
                income_minor: 1_000,
                in_base: null,
                in_base_gap: gap,
              }),
            ]}
            mode="base"
            baseCurrency="RUB"
          />,
        );
        return ROW_MARKERS.map((id) => screen.getByTestId(id).getAttribute("title") ?? "");
      }

      it("says an unrecorded purchase date, and does not promise it will resolve on its own", () => {
        for (const title of captionsFor("undated_lot")) {
          expect(title).toBe(CAPTION.undatedLot);
        }
        // The half that matters most: unlike the other three, nothing is
        // going to close this gap by itself — no rate backfill fixes a
        // missing DATE. But the date not being recoverable is not the same
        // as the position never computing (selling the lot, or re-entering
        // the transfer with a per-lot breakdown, both fix it), so the
        // caption must not claim "never" — only "not on its own".
        expect(CAPTION.undatedLot).not.toContain("никогда");
        expect(CAPTION.undatedLot).not.toContain("появится при обновлении");
        expect(CAPTION.undatedLot).toContain("уже неоткуда");
      });

      it("says a purchase day's missing rate, not an unrecorded date", () => {
        for (const title of captionsFor("no_rate_lot_date")) {
          expect(title).toBe(CAPTION.noRateLotDate);
          expect(title).not.toBe(CAPTION.undatedLot);
          expect(title).not.toBe(CAPTION.noRateIncomeDate);
          expect(title).not.toBe(CAPTION.noRateToday);
        }
      });

      it("says a payment day's missing rate, and not a purchase day's", () => {
        for (const title of captionsFor("no_rate_income_date")) {
          expect(title).toBe(CAPTION.noRateIncomeDate);
          expect(title).not.toBe(CAPTION.noRateLotDate);
          expect(title).not.toBe(CAPTION.noRateToday);
          expect(title).not.toBe(CAPTION.undatedLot);
        }
      });

      it("says today's missing rate, and not a historical day's", () => {
        for (const title of captionsFor("no_rate_today")) {
          expect(title).toBe(CAPTION.noRateToday);
          expect(title).not.toBe(CAPTION.noRateLotDate);
          expect(title).not.toBe(CAPTION.noRateIncomeDate);
          expect(title).not.toBe(CAPTION.undatedLot);
        }
      });

      it("promises the figure will appear for every closeable cause, and for no other", () => {
        // The whole user value of naming the term: «подтянется само» versus
        // «само не подтянется» — the closeable causes promise the figure will
        // appear on its own, the permanent one (undated_lot) promises only
        // that nothing will fix it automatically, never that it can't be
        // fixed at all. Asserted as a property of the four sentences rather
        // than case by case, so a new cause worded without either half fails
        // here.
        for (const gap of ["no_rate_lot_date", "no_rate_income_date", "no_rate_today"] as const) {
          const [cost] = captionsFor(gap);
          expect(cost).toContain("появится при обновлении курсов");
          expect(cost).not.toContain("никогда");
        }
        const [undated] = captionsFor("undated_lot");
        expect(undated).toContain("уже неоткуда");
      });
    });

    it("captions the row from in_base_gap alone, never from has_undated_lots", () => {
      // The row's caption has ONE source. Both fields derive from the same
      // server-side predicate and so cannot disagree in a real payload — these
      // two rows are therefore deliberately impossible ones, and they exist for
      // exactly that: they are the only way to prove WHICH field is read, and
      // two sources for one sentence is how the two eventually disagree.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              instrument: { ...makePosition().instrument, id: "instr-flag-on" },
              currency: "USD",
              has_undated_lots: true,
              in_base: null,
              in_base_gap: "no_rate_lot_date",
            }),
            makePosition({
              instrument: { ...makePosition().instrument, id: "instr-flag-off" },
              currency: "USD",
              has_undated_lots: false,
              in_base: null,
              in_base_gap: "undated_lot",
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      const [flagOn, flagOff] = screen.getAllByTestId("position-cost-not-converted");
      expect(flagOn).toHaveAttribute("title", CAPTION.noRateLotDate);
      expect(flagOff).toHaveAttribute("title", CAPTION.undatedLot);
    });

    it("falls back to the general phrase for a cause this build cannot name", () => {
      // A server newer than the client sends a value outside the union this
      // build knows. That must degrade to today's vague-but-true sentence —
      // not to a blank tooltip, not to a thrown render, and not to one of the
      // four specific sentences, which would name a cause the server did not
      // state. The second row is the same requirement for a payload with no
      // cause at all (an older server, or one breaking its own contract).
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              instrument: { ...makePosition().instrument, id: "instr-unknown-gap" },
              currency: "USD",
              in_base: null,
              // Deliberately outside the generated union — this is JSON off the
              // wire, typed by assertion rather than validated.
              in_base_gap: "no_rate_martian_holiday" as Position["in_base_gap"],
            }),
            makePosition({
              instrument: { ...makePosition().instrument, id: "instr-no-gap" },
              currency: "USD",
              in_base: null,
              in_base_gap: null,
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      for (const marker of screen.getAllByTestId("position-cost-not-converted")) {
        expect(marker).toHaveAttribute("title", CAPTION.general);
      }
      // Still marked, still showing the native figure: degrading the wording
      // must not degrade the disclosure.
      expect(screen.getAllByTestId("position-income-not-converted")).toHaveLength(2);
    });

    it("shows the plain native amounts with no indicator when the position's currency already is the base currency", () => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "RUB",
              market_value_currency: "RUB",
              cost_minor: 250_000,
              income_minor: 1_000,
              unrealized_pnl_minor: 25_000,
              in_base: null,
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      expect(norm(screen.getByTestId("position-cost").textContent ?? "")).toBe(
        norm(formatMinor(250_000, "RUB")),
      );
      expect(screen.queryByTestId("position-cost-not-converted")).not.toBeInTheDocument();
      expect(screen.queryByTestId("position-market-value-not-converted")).not.toBeInTheDocument();
      expect(screen.queryByTestId("position-profit-amount-not-converted")).not.toBeInTheDocument();
      expect(screen.queryByTestId("position-income-not-converted")).not.toBeInTheDocument();
    });

    it("gives the valuation its own cause even when the position's currency already is the base currency", () => {
      // The contract publishes market_value_gap independently of whether
      // `currency` already equals the space's base one (server-side pinned by
      // internal/portfolio/http_position_in_base_gap_test.go,
      // TestPositionMarketValueGapPublishedOnABaseCurrencyPosition — a bond's
      // EUR face value stuck on a RUB position of a RUB space). in_base_gap
      // stays null here: cost/income/profit are already in the base currency,
      // so rowGapTitle's fallback for this row is the vague general phrase —
      // making this the one row where the valuation's own, nearer cause has
      // to outrank the fallback rather than merely outrank a named row cause.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "RUB",
              market_value_currency: "EUR",
              cost_minor: 250_000,
              income_minor: 1_000,
              unrealized_pnl_minor: 25_000,
              in_base: null,
              in_base_gap: null,
              market_value_gap: "no_rate_valuation_currency",
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      // Cost/income/profit need no conversion at all — no marker on any of
      // them.
      expect(screen.queryByTestId("position-cost-not-converted")).not.toBeInTheDocument();
      expect(screen.queryByTestId("position-profit-amount-not-converted")).not.toBeInTheDocument();
      expect(screen.queryByTestId("position-income-not-converted")).not.toBeInTheDocument();

      // The valuation alone carries a marker, and it names its OWN cause —
      // not the general phrase the row would otherwise fall back to.
      const valuationMarker = screen.getByTestId("position-market-value-not-converted");
      expect(valuationMarker).toHaveAttribute("title", CAPTION.valuationCurrency);
      expect(valuationMarker.getAttribute("title")).not.toBe(CAPTION.general);
    });

    it("claims today's rate only for the market value, and names the historical rates behind cost, income and profit", () => {
      // in_base.rate_on is the rate date behind the market VALUATION and
      // nothing else. The ruble basis is built lot by lot at each purchase
      // date's rate, and income operation by operation at each operation
      // date's rate, so that single date describes exactly one of the four
      // cells. Letting the default MoneyCell wording ("converted at today's
      // rate, on <date>") stand under the other three would claim a
      // conversion that never happened, on a date that has nothing to do
      // with them.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              in_base: {
                cost_minor: 2_275_000,
                market_value_minor: 2_780_050,
                unrealized_pnl_minor: 227_500,
                income_minor: 9_100,
                currency: "RUB",
                rate_on: "2026-07-19",
              },
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      expect(screen.getByTestId("position-market-value")).toHaveAttribute(
        "title",
        "Пересчитано по текущему курсу (на 19.07.2026)",
      );
      expect(screen.getByTestId("position-cost")).toHaveAttribute(
        "title",
        "Пересчитано по курсам на даты покупок, а не по текущему",
      );
      expect(screen.getByTestId("position-income")).toHaveAttribute(
        "title",
        "Пересчитано по курсам на даты выплат, а не по текущему",
      );
      expect(screen.getByTestId("position-profit-amount")).toHaveAttribute(
        "title",
        "Оценка по текущему курсу минус стоимость по курсам на даты покупок — поэтому включает изменение курса",
      );
      // The valuation's rate date must not leak into the three tooltips that
      // it says nothing about.
      for (const testId of ["position-cost", "position-income", "position-profit-amount"]) {
        expect(screen.getByTestId(testId).getAttribute("title")).not.toMatch(/19\.07\.2026/);
      }
      // Tooltips only — no date ever becomes cell text.
      expect(screen.queryByText(/19\.07\.2026/)).not.toBeInTheDocument();
    });

    it("never calls the missing figure the account's, whichever gap fired", () => {
      // A position row's amounts are in the position's / quote's / bond face
      // value's currency — calling that "the account's currency" would point
      // the user at the wrong thing. The four named-gap sentences (#66) no
      // longer name any currency at all in their closing clause since the
      // wording fix that scoped it to the row (it says only that no figure in
      // the row reached the BASE currency, true regardless of which currency
      // it stayed in) — so "не называет счётом" is what is left to assert for
      // them. The general fallback is the only one of the five that still
      // names a currency, and it must still name the right one.
      for (const gap of [
        "undated_lot",
        "no_rate_lot_date",
        "no_rate_income_date",
        "no_rate_today",
      ] as const) {
        cleanup();
        wrap(
          <PositionsTable
            positions={[makePosition({ currency: "USD", in_base: null, in_base_gap: gap })]}
            mode="base"
            baseCurrency="RUB"
          />,
        );

        const title = screen.getByTestId("position-cost-not-converted").getAttribute("title") ?? "";
        expect(title).not.toContain("счёт");
        expect(title).not.toContain("счет");
      }

      cleanup();
      wrap(
        <PositionsTable
          positions={[makePosition({ currency: "USD", in_base: null, in_base_gap: null })]}
          mode="base"
          baseCurrency="RUB"
        />,
      );
      const generalTitle =
        screen.getByTestId("position-cost-not-converted").getAttribute("title") ?? "";
      expect(generalTitle).toContain("в исходной валюте");
      expect(generalTitle).not.toContain("счёт");
      expect(generalTitle).not.toContain("счет");
    });

    it("shows a valuation in a third currency honestly, in its own currency, when in_base cannot express it", () => {
      // The backend publishes in_base.market_value_minor only when the
      // valuation is in the position's own currency: here a bond's EUR face
      // value on a USD position, with no EUR rate anywhere, so converting it
      // with the position's USD->RUB rate would be a silently wrong number
      // (and, equal to the converted cost, would read as "profit exactly
      // zero"). cost/income still convert normally.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              cost_minor: 100_000,
              income_minor: 0,
              market_value_minor: 100_000,
              market_value_currency: "EUR",
              unrealized_pnl_minor: null,
              // The valuation's own cause, published by the server where the
              // conversion actually failed — never re-derived here by comparing
              // the two currency codes. in_base_gap stays null: the OBJECT was
              // struck, only this one figure inside it was not.
              market_value_gap: "no_rate_valuation_currency",
              in_base: {
                cost_minor: 9_000_000,
                market_value_minor: null,
                unrealized_pnl_minor: null,
                income_minor: 0,
                currency: "RUB",
                // Null with the valuation it is the date of: this object holds
                // no figure struck at a single rate (see PositionInBase.rate_on
                // in the API contract).
                rate_on: null,
              },
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      // Cost converted, as usual — and still saying so: its wording names the
      // purchase-day rates, which do not depend on a rate_on the object no
      // longer carries.
      expect(norm(screen.getByTestId("position-cost").textContent ?? "")).toBe(
        norm(formatMinor(9_000_000, "RUB")),
      );
      expect(screen.getByTestId("position-cost")).toHaveAttribute(
        "title",
        "Пересчитано по курсам на даты покупок, а не по текущему",
      );
      // The valuation stays the real 1 000,00 € with a marker — never
      // 90 000,00 ₽ (that same figure times the USD rate) passed off as base
      // currency, and never a dash or a zero.
      const marketValue = screen.getByTestId("position-market-value");
      expect(norm(marketValue.textContent ?? "")).toContain(norm(formatMinor(100_000, "EUR")));
      expect(marketValue.textContent).not.toMatch(/₽/);
      // Issue #42: the marker must name the cause that IS the cause. The rate
      // that is missing is the link from EUR to the position's USD, without
      // which the row has nothing to compare the valuation against, and the
      // backend withholds the base-currency figure with it. It is missing for
      // this ONE figure: the cost in the very same row converted into rubles
      // above. "Нет курса" is the row's marker and would say the row could not
      // be converted at all, which the cell beside it disproves.
      const valuationMarker = screen.getByTestId("position-market-value-not-converted");
      expect(valuationMarker).toHaveAttribute("title", CAPTION.valuationCurrency);
      expect(valuationMarker.getAttribute("title")).not.toBe(CAPTION.general);
      // Profit is derived from that valuation, so it stays an honest dash.
      expect(screen.getByTestId("position-profit-dash")).toHaveTextContent("—");
      expect(screen.queryByTestId("position-profit-amount")).not.toBeInTheDocument();
    });

    it("keeps the row's own reason on the cells the valuation's reason is not true of", () => {
      // The twin of the test above, and the combination the contract calls out
      // as the reason market_value_gap is a SECOND field rather than one more
      // value of in_base_gap: both are set, and both are true, of different
      // cells of one row. The valuation is stuck in a third currency; the cost
      // and the income are in the position's own currency and were stopped by
      // something else entirely — a dateless lot, which is permanent and which
      // the valuation's own sentence says nothing about.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              market_value_minor: 100_000,
              market_value_currency: "EUR",
              unrealized_pnl_minor: null,
              in_base: null,
              in_base_gap: "undated_lot",
              market_value_gap: "no_rate_valuation_currency",
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      // The nearer, cell-specific cause wins on the cell it belongs to.
      expect(screen.getByTestId("position-market-value-not-converted")).toHaveAttribute(
        "title",
        CAPTION.valuationCurrency,
      );
      // ...and does not leak onto the cells it says nothing about.
      expect(screen.getByTestId("position-cost-not-converted")).toHaveAttribute(
        "title",
        CAPTION.undatedLot,
      );
      expect(screen.getByTestId("position-income-not-converted")).toHaveAttribute(
        "title",
        CAPTION.undatedLot,
      );
    });

    it("gives the valuation the row's own cause when the valuation itself converted fine", () => {
      // The decision this task had to make. in_base_gap says the row stopped on
      // a purchase day's missing rate; market_value_gap is null, so nothing
      // about THIS figure's own conversion failed — the whole in_base object
      // was withheld, the valuation with it. The row's sentence is what is true
      // here, and it is worded to say so («ни одно число этой строки не
      // показано в базовой валюте»). What must not appear is the valuation's
      // own sentence, which
      // would blame a third currency that is not in play, or a bare «нет
      // курса», which reads as "today's rate is missing" over a figure whose
      // today-rate the server never even got to ask for.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              in_base: null,
              in_base_gap: "no_rate_lot_date",
              market_value_gap: null,
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      const valuationMarker = screen.getByTestId("position-market-value-not-converted");
      expect(valuationMarker).toHaveAttribute("title", CAPTION.noRateLotDate);
      expect(valuationMarker.getAttribute("title")).not.toBe(CAPTION.valuationCurrency);
      expect(valuationMarker.getAttribute("title")).not.toBe(CAPTION.general);
      expect(valuationMarker.getAttribute("title")).not.toBe(CAPTION.noRateToday);
    });

    it("captions the valuation from market_value_gap, never from the two currency codes", () => {
      // The valuation's caption has ONE source too, and this row is the only
      // way to prove which: market_value_currency differs from the position's,
      // yet the server reports no gap of the valuation's own. That payload is
      // impossible today — the server sets the flag in the very branch where
      // the conversion into the position's currency fails, which is the only
      // way the two codes can come apart — and it is impossible on purpose:
      // re-deriving the cause from the codes is a SECOND answer waiting to
      // disagree with the server's, and the contract already warns it will
      // (a rate table quoted in something other than RUB makes such a
      // valuation convertible, and the codes would still differ).
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              market_value_minor: 100_000,
              market_value_currency: "EUR",
              unrealized_pnl_minor: null,
              in_base: null,
              in_base_gap: "no_rate_income_date",
              market_value_gap: null,
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      const valuationMarker = screen.getByTestId("position-market-value-not-converted");
      expect(valuationMarker).toHaveAttribute("title", CAPTION.noRateIncomeDate);
      expect(valuationMarker.getAttribute("title")).not.toBe(CAPTION.valuationCurrency);
    });

    it("falls back to the row's cause for a valuation gap this build cannot name", () => {
      // Same requirement as the row's unknown value, answered one step
      // differently: a valuation whose own cause this build cannot name is
      // still covered by whatever stopped the row, which the server did name —
      // so the cell degrades to that rather than to blankness. When the row has
      // no named cause either, that fallback is itself the general phrase.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              instrument: { ...makePosition().instrument, id: "instr-row-named" },
              currency: "USD",
              market_value_minor: 100_000,
              market_value_currency: "EUR",
              unrealized_pnl_minor: null,
              in_base: null,
              in_base_gap: "undated_lot",
              market_value_gap:
                "no_rate_lunar_settlement" as Position["market_value_gap"],
            }),
            makePosition({
              instrument: { ...makePosition().instrument, id: "instr-row-unnamed" },
              currency: "USD",
              market_value_minor: 100_000,
              market_value_currency: "EUR",
              unrealized_pnl_minor: null,
              in_base: null,
              in_base_gap: null,
              market_value_gap:
                "no_rate_lunar_settlement" as Position["market_value_gap"],
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      const [rowNamed, rowUnnamed] = screen.getAllByTestId(
        "position-market-value-not-converted",
      );
      expect(rowNamed).toHaveAttribute("title", CAPTION.undatedLot);
      expect(rowUnnamed).toHaveAttribute("title", CAPTION.general);
    });

    it("never calls the base currency a third one, even on a position denominated in another", () => {
      // The server sets market_value_gap wherever the valuation failed to reach
      // the POSITION's currency — which is also true of a valuation that came
      // out in the base currency, where «третья валюта» would be plainly false:
      // the base currency is the second one and the figure needs no chain to
      // reach it. The sentence stays unshown anyway, and this pins WHY: it
      // rides on the marker, the marker appears only when a figure could not be
      // converted, and a figure already denominated in the base currency has
      // nothing to convert. The invariant was left to be inferred; here it is
      // stated, so a later change to either half fails instead of producing a
      // sentence about a currency that is not third and not missing a rate.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              cost_minor: 100_000,
              market_value_minor: 9_000_000,
              // A bond priced off a face value denominated in the base
              // currency, held on a dollar position.
              market_value_currency: "RUB",
              unrealized_pnl_minor: null,
              market_value_gap: "no_rate_valuation_currency",
              in_base: null,
              in_base_gap: "no_rate_lot_date",
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      const marketValue = screen.getByTestId("position-market-value");
      expect(norm(marketValue.textContent ?? "")).toContain(norm(formatMinor(9_000_000, "RUB")));
      expect(screen.queryByTestId("position-market-value-not-converted")).not.toBeInTheDocument();
      expect(screen.queryByTitle(/в третьей валюте/)).not.toBeInTheDocument();
      // The cells that ARE in the position's currency keep the row's own
      // reason, which here is a purchase day's missing rate and nothing to do
      // with chains.
      expect(screen.getByTestId("position-cost-not-converted")).toHaveAttribute(
        "title",
        CAPTION.noRateLotDate,
      );
    });

    it("still shows the no-quote / currency-mismatch dashes regardless of mode — unaffected by base conversion", () => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              market_value_minor: null,
              market_value_currency: null,
              unrealized_pnl_minor: null,
              in_base: null,
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      expect(screen.getByTestId("position-no-quote")).toHaveTextContent("—");
      expect(screen.getByTestId("position-profit-dash")).toHaveTextContent("—");
    });

    it("names the currency the return percentage is measured in, in both modes", () => {
      // The percentage is a bare "+10,0 %" with no currency anywhere near it,
      // yet it means two different things in the two modes. Naming the
      // currency is what lets a reader tell "the share went up 10 %" from
      // "the holding grew my base currency by 10 %".
      const position = makePosition({
        currency: "USD",
        cost_minor: 250_000,
        unrealized_pnl_minor: 25_000,
        in_base: {
          cost_minor: 2_000_000,
          market_value_minor: 2_200_000,
          unrealized_pnl_minor: 200_000,
          income_minor: 0,
          currency: "RUB",
          rate_on: "2026-07-20",
        },
      });

      const { rerender } = wrap(
        <PositionsTable positions={[position]} mode="native" baseCurrency="RUB" />,
      );
      expect(screen.getByTestId("position-profit-percent")).toHaveAttribute(
        "title",
        "Доходность в USD. В другой валюте ответ другой — вплоть до противоположного знака: это два разных вопроса, а не расхождение",
      );

      rerender(<PositionsTable positions={[position]} mode="base" baseCurrency="RUB" />);
      expect(screen.getByTestId("position-profit-percent")).toHaveAttribute(
        "title",
        "Доходность в RUB. В другой валюте ответ другой — вплоть до противоположного знака: это два разных вопроса, а не расхождение",
      );
    });

    it("names the currency the figures are actually in, not the one the mode asked for", () => {
      // Base mode with no fx rate leaves both cost and profit in the
      // position's own currency, so the ratio is a USD one no matter what
      // the mode wanted. A label taken from the mode rather than from the
      // resolved figures would announce a ruble return over two dollar
      // numbers — the one case where the two sources disagree, and the
      // reason the label follows the numbers.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              cost_minor: 250_000,
              unrealized_pnl_minor: 25_000,
              in_base: null,
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      expect(screen.getByTestId("position-profit-percent")).toHaveAttribute(
        "title",
        "Доходность в USD. В другой валюте ответ другой — вплоть до противоположного знака: это два разных вопроса, а не расхождение",
      );
    });

    it("flips the profit's sign and colour with the display mode: a gain in the position's currency, a loss in the base one", () => {
      // THIS IS NOT A BUG — do not "fix" it by making the two agree.
      //
      // It is the owner's explicit decision (2026-07-29), and the whole point
      // of the ruble cost basis: the base-currency return includes the
      // currency's own move, the position-currency return does not. When the
      // ruble strengthens hard enough, the two honest answers disagree in
      // sign, and both must be shown as they are — a version that kept the
      // signs in step would be hiding the currency loss from its owner.
      //
      // Numbers are lifted verbatim from the backend test that pins the same
      // scenario (TestPositionInBaseProfitInPositionCurrencyLossInBase in
      // internal/portfolio/http_position_in_base_test.go), so the two
      // fixtures tell one story: bought 10 @ $100 when USD->RUB was 100,
      // quoted at $110 today when it is 50.
      //   USD: 110 000 - 100 000 =    +10 000  (+10,0 %)
      //   RUB: 5 500 000 - 10 000 000 = -4 500 000 (-45,0 %)
      const position = makePosition({
        currency: "USD",
        cost_minor: 100_000,
        income_minor: 0,
        market_value_minor: 110_000,
        market_value_currency: "USD",
        price: "110",
        unrealized_pnl_minor: 10_000,
        in_base: {
          cost_minor: 10_000_000,
          market_value_minor: 5_500_000,
          unrealized_pnl_minor: -4_500_000,
          income_minor: 0,
          currency: "RUB",
          rate_on: "2026-07-20",
        },
      });

      const { rerender } = wrap(
        <PositionsTable positions={[position]} mode="native" baseCurrency="RUB" />,
      );

      const nativeAmount = screen.getByTestId("position-profit-amount");
      expect(norm(nativeAmount.textContent ?? "")).toBe(norm(formatMinor(10_000, "USD")));
      expect(nativeAmount.className).toContain("text-emerald-500");
      expect(norm(screen.getByTestId("position-profit-percent").textContent ?? "")).toBe(
        norm("+10,0 %"),
      );

      rerender(<PositionsTable positions={[position]} mode="base" baseCurrency="RUB" />);

      const baseAmount = screen.getByTestId("position-profit-amount");
      expect(norm(baseAmount.textContent ?? "")).toBe(norm(formatMinor(-4_500_000, "RUB")));
      expect(baseAmount.className).toContain("text-red-500");
      expect(baseAmount.className).not.toContain("text-emerald-500");
      expect(norm(screen.getByTestId("position-profit-percent").textContent ?? "")).toBe(
        norm("-45,0 %"),
      );
    });
  });
});
