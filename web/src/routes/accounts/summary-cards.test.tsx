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
    wrap(<SummaryCards summary={summary} />);

    expect(norm(screen.getByTestId("summary-total-amount").textContent ?? "")).toBe(
      norm(formatMinorCompact(summary.total_in_base_minor!, summary.base_currency)),
    );
    expect(screen.queryByText(/не учтены/)).not.toBeInTheDocument();
    expect(screen.queryByText(/курс от/)).not.toBeInTheDocument();
  });

  it("lists unconverted currencies in the context row", () => {
    wrap(<SummaryCards summary={makeSummary({ unconverted: ["KZT"] })} />);

    expect(screen.getByText(/не учтены:.*KZT/)).toBeInTheDocument();
  });

  it("shows the rates date when it is older than yesterday", () => {
    wrap(<SummaryCards summary={makeSummary({ rates_on: "2026-07-20" })} />);

    expect(screen.getByText(/курс от 20\.07\.2026/)).toBeInTheDocument();
  });

  it("hides the rates date when it is today or yesterday", () => {
    const today = localToday();
    wrap(<SummaryCards summary={makeSummary({ rates_on: today })} />);
    expect(screen.queryByText(/курс от/)).not.toBeInTheDocument();
  });

  it("shows the no-total explanation instead of a zero amount when nothing could be converted", () => {
    wrap(
      <SummaryCards
        summary={makeSummary({ total_in_base_minor: null, unconverted: ["RUB"], rates_on: null })}
      />,
    );

    expect(screen.getByText("Нет курсов для пересчета")).toBeInTheDocument();
    expect(screen.queryByTestId("summary-total-amount")).not.toBeInTheDocument();
    expect(screen.queryByText(formatMinorCompact(0, "RUB"))).not.toBeInTheDocument();
  });
});
