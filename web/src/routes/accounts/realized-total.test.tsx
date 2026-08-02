import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { RealizedTotal, sumRealized } from "./realized-total";
import type { Position } from "@/api/positions";

// NBSP-insensitive compare: Intl.NumberFormat uses non-breaking spaces
// (matches the helper in positions-table.test.tsx / money.test.ts).
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

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
    // The ordinary case for both gap flags: every lot came from a buy, and a
    // buy always knows its own date. Tests about the other cases set them.
    has_undated_lots: false,
    has_undated_realizations: false,
    ...overrides,
  };
}

// A position's in_base block, with the fields this figure never reads left at
// values that are irrelevant to it on purpose: the realized total must be
// stopped by its own terms, not by the valuation's or the basis's.
function inBase(overrides: Partial<NonNullable<Position["in_base"]>> = {}) {
  return {
    cost_minor: 20_000_000,
    income_minor: 0,
    realized_pnl_minor: 0,
    currency: "RUB",
    rate_on: "2026-07-20",
    ...overrides,
  };
}

describe("sumRealized", () => {
  it("adds the positions of one currency into a single figure", () => {
    const result = sumRealized(
      [
        makePosition({ realized_pnl_minor: 10_000 }),
        makePosition({ realized_pnl_minor: 2_500 }),
      ],
      "native",
      "RUB",
    );

    expect(result).toEqual({ totals: [{ currency: "USD", amountMinor: 12_500 }], gap: null });
  });

  it("keeps two currencies apart instead of adding them into one number", () => {
    // Dollars and euros are not addable, and a sum of the two integers would
    // be a number denominated in nothing at all.
    const result = sumRealized(
      [
        makePosition({ currency: "USD", realized_pnl_minor: 10_000 }),
        makePosition({ currency: "EUR", realized_pnl_minor: 2_500 }),
      ],
      "native",
      "RUB",
    );

    expect(result).toEqual({
      totals: [
        { currency: "EUR", amountMinor: 2_500 },
        { currency: "USD", amountMinor: 10_000 },
      ],
      gap: null,
    });
  });

  it("adds the base-currency figures, not the position-currency ones, in base mode", () => {
    // The two are different numbers on purpose: the base figure carries the
    // currency's move between purchase and sale (see PositionInBase in the
    // API contract), so picking the wrong one is a silently wrong total.
    const result = sumRealized(
      [
        makePosition({
          realized_pnl_minor: 10_000,
          in_base: inBase({ realized_pnl_minor: 900_000 }),
        }),
        makePosition({
          realized_pnl_minor: 2_500,
          in_base: inBase({ realized_pnl_minor: 100_000 }),
        }),
      ],
      "base",
      "RUB",
    );

    expect(result).toEqual({ totals: [{ currency: "RUB", amountMinor: 1_000_000 }], gap: null });
  });

  it("counts a position already in the base currency by its own figure", () => {
    // Such a position carries no in_base at all (nothing to convert), and
    // skipping it — or calling it unknown — would under-report the total.
    const result = sumRealized(
      [
        makePosition({ currency: "RUB", realized_pnl_minor: 5_000, in_base: null }),
        makePosition({
          currency: "USD",
          realized_pnl_minor: 10_000,
          in_base: inBase({ realized_pnl_minor: 900_000 }),
        }),
      ],
      "base",
      "RUB",
    );

    expect(result).toEqual({ totals: [{ currency: "RUB", amountMinor: 905_000 }], gap: null });
  });

  it("refuses the base-currency sum when one position's figure is unknown", () => {
    // Adding the known ones and calling the result "the total" would publish
    // a number smaller than the truth and indistinguishable from a real one.
    const result = sumRealized(
      [
        makePosition({ in_base: inBase({ realized_pnl_minor: 900_000 }) }),
        makePosition({
          has_undated_realizations: true,
          in_base: inBase({ realized_pnl_minor: null }),
        }),
      ],
      "base",
      "RUB",
    );

    expect(result).toEqual({ totals: [], gap: "undated" });
  });

  it("blames the missing rate when every purchase date was recorded", () => {
    const result = sumRealized(
      [makePosition({ in_base: inBase({ realized_pnl_minor: null }) })],
      "base",
      "RUB",
    );

    expect(result.gap).toBe("no_rate");
  });

  it("does not blame the held undated lots for a realized figure a missing rate stopped", () => {
    // has_undated_lots speaks about the lots still HELD; the parcel behind a
    // realized figure has already been sold and is never among them. Reading
    // it as the explanation here would name a cause that is not the cause and
    // promise a figure that will never come.
    const result = sumRealized(
      [
        makePosition({
          has_undated_lots: true,
          has_undated_realizations: false,
          in_base: inBase({ realized_pnl_minor: null }),
        }),
      ],
      "base",
      "RUB",
    );

    expect(result.gap).toBe("no_rate");
  });

  it("blames the unrecorded dates when an undated held lot nulled the whole conversion block", () => {
    // No in_base at all: the realized figure is unavailable because a date
    // nobody wrote down stopped the object it lives in, and that never
    // resolves — "нет курса" would be false about it.
    const result = sumRealized(
      [makePosition({ has_undated_lots: true, in_base: null })],
      "base",
      "RUB",
    );

    expect(result.gap).toBe("undated");
  });

  it("blames the missing rate when the whole conversion block is missing and no date is", () => {
    const result = sumRealized([makePosition({ in_base: null })], "base", "RUB");

    expect(result.gap).toBe("no_rate");
  });

  it("names both causes when different positions are stopped by different gaps", () => {
    const result = sumRealized(
      [
        makePosition({ has_undated_realizations: true, in_base: inBase({ realized_pnl_minor: null }) }),
        makePosition({ in_base: null }),
      ],
      "base",
      "RUB",
    );

    expect(result.gap).toBe("both");
  });

  it("has no gaps in the positions' own currency, whatever the conversion is missing", () => {
    // realized_pnl_minor in the position's own currency is always published
    // (see the API contract): an unrecorded purchase date costs no money and
    // no shares, so this mode always has a figure to show.
    const result = sumRealized(
      [
        makePosition({
          realized_pnl_minor: 7_000,
          has_undated_realizations: true,
          has_undated_lots: true,
          in_base: null,
        }),
      ],
      "native",
      "RUB",
    );

    expect(result).toEqual({ totals: [{ currency: "USD", amountMinor: 7_000 }], gap: null });
  });
});

describe("RealizedTotal", () => {
  it("shows the summed figure under its own label", () => {
    render(
      <RealizedTotal
        positions={[
          makePosition({ realized_pnl_minor: 10_000 }),
          makePosition({ realized_pnl_minor: 2_500 }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByText("Зафиксировано")).toBeInTheDocument();
    expect(norm(screen.getByTestId("realized-total-amounts").textContent ?? "")).toContain(
      "125,00 $",
    );
  });

  it("explains in a tooltip how this differs from the profit on paper", () => {
    // Which rates stand behind the two ends of the figure is exactly the
    // detail that belongs in a tooltip rather than in the text of a screen
    // full of numbers (the owner's standing rule).
    render(
      <RealizedTotal positions={[makePosition()]} mode="native" baseCurrency="RUB" />,
    );

    const hint = screen.getByTestId("realized-total-label").getAttribute("title") ?? "";
    expect(hint).toContain("на дни покупок");
    expect(hint).toContain("на день продажи");
    expect(hint).toContain("изменение курса");
    // The label itself stays a label; the mechanics are not printed as text.
    expect(screen.getByTestId("realized-total-label").textContent).toBe("Зафиксировано");
  });

  it("shows each currency's figure separately rather than one meaningless number", () => {
    render(
      <RealizedTotal
        positions={[
          makePosition({ currency: "USD", realized_pnl_minor: 10_000 }),
          makePosition({ currency: "EUR", realized_pnl_minor: 2_500 }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const shown = norm(screen.getByTestId("realized-total-amounts").textContent ?? "");
    expect(shown).toContain("100,00 $");
    expect(shown).toContain("25,00 €");
  });

  it("shows no base-currency sum, and says the dates are what stopped it", () => {
    render(
      <RealizedTotal
        positions={[
          makePosition({ in_base: inBase({ realized_pnl_minor: 900_000 }) }),
          makePosition({
            has_undated_realizations: true,
            in_base: inBase({ realized_pnl_minor: null }),
          }),
        ]}
        mode="base"
        baseCurrency="RUB"
      />,
    );

    expect(screen.queryByTestId("realized-total-amounts")).not.toBeInTheDocument();
    const gap = screen.getByTestId("realized-total-gap");
    expect(gap.textContent).toContain("даты покупок");
    // Saying "нет курса" here would name a cause that will never be the true
    // one and promise a number that is never coming.
    expect(gap.textContent).not.toContain("курс");
    expect(gap.getAttribute("title")).toContain("никогда");
  });

  it("says the rate is what stopped it when no purchase date is missing", () => {
    render(
      <RealizedTotal
        positions={[makePosition({ in_base: inBase({ realized_pnl_minor: null }) })]}
        mode="base"
        baseCurrency="RUB"
      />,
    );

    const gap = screen.getByTestId("realized-total-gap");
    expect(gap.textContent).toContain("курс");
    expect(gap.textContent).not.toContain("даты покупок");
    // A rate that has not been fetched yet is a gap that closes on its own.
    expect(gap.getAttribute("title")).toContain("обнов");
  });

  it("names both causes when different positions are stopped by different gaps", () => {
    render(
      <RealizedTotal
        positions={[
          makePosition({
            has_undated_realizations: true,
            in_base: inBase({ realized_pnl_minor: null }),
          }),
          makePosition({ in_base: null }),
        ]}
        mode="base"
        baseCurrency="RUB"
      />,
    );

    const gap = screen.getByTestId("realized-total-gap");
    expect(gap.textContent).toContain("даты покупок");
    expect(gap.textContent).toContain("курс");
  });

  it("renders nothing when the account has no positions at all", () => {
    render(<RealizedTotal positions={[]} mode="native" baseCurrency="RUB" />);

    expect(screen.queryByTestId("realized-total")).not.toBeInTheDocument();
  });
});
