import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { SummaryCards } from "./summary-cards";
import type { Summary } from "@/api/accounts";
import { formatMinorCompact } from "@/lib/money";
import { localToday } from "@/lib/dates";

// SummaryCards is a pure presentational component (summary comes in as a
// prop), so a bare render is enough — no QueryClientProvider needed.
function wrap(ui: ReactElement) {
  return render(ui);
}

// NBSP-insensitive compare: Intl.NumberFormat uses non-breaking spaces
// (matches the helper in money.test.ts).
const norm = (s: string) => s.replace(/[  ]/g, " ");

function makeSummary(overrides: Partial<Summary> = {}): Summary {
  return {
    totals: [
      { currency: "RUB", assets_minor: 123_456_700, liabilities_minor: 0, net_minor: 123_456_700 },
    ],
    base_currency: "RUB",
    total_in_base_minor: 123_456_700,
    unconverted: [],
    rates_on: localToday(),
    ...overrides,
  };
}

describe("SummaryCards", () => {
  it("shows the total with no context row when everything converted and rates are fresh", () => {
    const summary = makeSummary();
    wrap(<SummaryCards summary={summary} mode="native" />);

    expect(norm(screen.getByTestId("summary-total-amount").textContent ?? "")).toBe(
      norm(formatMinorCompact(summary.total_in_base_minor!, summary.base_currency)),
    );
    expect(screen.queryByText(/не учтены/)).not.toBeInTheDocument();
    expect(screen.queryByText(/курс от/)).not.toBeInTheDocument();
    // Fresh rates -> no stale-rates indicator icon at all.
    expect(screen.queryByTestId("summary-rates-stale-icon")).not.toBeInTheDocument();
  });

  it("lists unconverted currencies in the context row", () => {
    wrap(<SummaryCards summary={makeSummary({ unconverted: ["KZT"] })} mode="native" />);

    expect(screen.getByText(/не учтены:.*KZT/)).toBeInTheDocument();
  });

  it("shows the rates date only in the stale-rates icon's tooltip, not as text", () => {
    wrap(<SummaryCards summary={makeSummary({ rates_on: "2026-07-20" })} mode="native" />);

    expect(screen.queryByText(/курс от/)).not.toBeInTheDocument();
    const icon = screen.getByTestId("summary-rates-stale-icon");
    expect(icon).toHaveAttribute("title", "курс от 20.07.2026");
  });

  it("hides the rates date and the stale-rates icon when rates are today or yesterday", () => {
    const today = localToday();
    wrap(<SummaryCards summary={makeSummary({ rates_on: today })} mode="native" />);
    expect(screen.queryByText(/курс от/)).not.toBeInTheDocument();
    expect(screen.queryByTestId("summary-rates-stale-icon")).not.toBeInTheDocument();
  });

  it("shows the no-total explanation instead of a zero amount when nothing could be converted", () => {
    wrap(
      <SummaryCards
        summary={makeSummary({ total_in_base_minor: null, unconverted: ["RUB"], rates_on: null })}
        mode="native"
      />,
    );

    expect(screen.getByText("Нет курсов для пересчета")).toBeInTheDocument();
    expect(screen.queryByTestId("summary-total-amount")).not.toBeInTheDocument();
    expect(screen.queryByText(formatMinorCompact(0, "RUB"))).not.toBeInTheDocument();
  });

  it("renders a zero total amount (0 ₽) when total_in_base_minor is 0", () => {
    wrap(<SummaryCards summary={makeSummary({ total_in_base_minor: 0 })} mode="native" />);

    const amount = screen.getByTestId("summary-total-amount");
    expect(amount).toBeInTheDocument();
    expect(norm(amount.textContent ?? "")).toBe(norm(formatMinorCompact(0, "RUB")));
    // Ensure it does NOT show the "no total" explanation
    expect(screen.queryByText("Нет курсов для пересчета")).not.toBeInTheDocument();
  });

  it("does not print a coloured zero on the headline card for a total that is not zero", () => {
    // #107 where it actually hurts. The total card pairs the figure with
    // signClass, so forty kopecks used to be shown as «0 ₽» in the green of a
    // gain: a zero and a sign, contradicting each other, on the largest number
    // on the screen. Asserted against the zero rendering as a whole string —
    // looking for the character "0" would match «1 385 000 ₽» just as well.
    wrap(<SummaryCards summary={makeSummary({ total_in_base_minor: 40 })} mode="native" />);

    const amount = screen.getByTestId("summary-total-amount");
    expect(norm(amount.textContent ?? "")).toBe("0,40 ₽");
    expect(amount.textContent).not.toBe(formatMinorCompact(0, "RUB"));
    // The colour is right and stays: this total IS a gain, and the number now
    // agrees with it.
    expect(amount.className).toContain("emerald");
  });

  it("omits the rates-date fragment when rates_on is unparseable", () => {
    wrap(
      <SummaryCards summary={makeSummary({ rates_on: "garbage" })} mode="native" />,
    );

    // Ensure the rate date fragment is not present (no "курс от" text), and
    // no stale-rates icon either since there's no valid date to show in it.
    expect(screen.queryByText(/курс от/)).not.toBeInTheDocument();
    expect(screen.queryByTestId("summary-rates-stale-icon")).not.toBeInTheDocument();
  });

  it("shows the per-currency cards in native mode", () => {
    wrap(<SummaryCards summary={makeSummary()} mode="native" />);

    expect(screen.getByText("Итого в RUB")).toBeInTheDocument();
  });

  it("hides the per-currency cards in base mode, but keeps the total", () => {
    const summary = makeSummary();
    wrap(<SummaryCards summary={summary} mode="base" />);

    expect(screen.queryByText("Итого в RUB")).not.toBeInTheDocument();
    expect(screen.getByTestId("summary-total-amount")).toBeInTheDocument();
    expect(norm(screen.getByTestId("summary-total-amount").textContent ?? "")).toBe(
      norm(formatMinorCompact(summary.total_in_base_minor!, summary.base_currency)),
    );
  });
});
