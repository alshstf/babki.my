import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { SummaryCards } from "./summary-cards";
import type { Summary } from "@/api/accounts";
import { formatMinor, formatMinorCompact } from "@/lib/money";
import { localToday } from "@/lib/dates";

// A presentational component: a bare render is enough.
function wrap(ui: ReactElement) {
  return render(ui);
}

// NBSP-insensitive compare.
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
    journal: {
      accounts: 0,
      differing: 0,
      differing_difference_minor: 0,
      pinned_to_balance: 0,
      unpriced_positions: 0,
      not_traded_positions: 0,
      full_difference_minor: 0,
    },
    ...overrides,
  };
}

// The stale-rates tooltip, pinned whole. Summary.rates_on is the oldest
// rate date across the converted currencies (ConvertMany keeps the
// minimum), one rate per currency.
const RATES_ON =
  "Валюты пересчитаны каждая по своему курсу, и самый старый из этих курсов — от 20.07.2026";

describe("SummaryCards", () => {
  it("shows the total with no context row when everything converted and rates are fresh", () => {
    const summary = makeSummary();
    wrap(<SummaryCards summary={summary} mode="native" />);

    expect(norm(screen.getByTestId("summary-total-amount").textContent ?? "")).toBe(
      norm(formatMinorCompact(summary.total_in_base_minor!, summary.base_currency)),
    );
    expect(screen.queryByText(/не учтены/)).not.toBeInTheDocument();
    expect(screen.queryByText(/курс/)).not.toBeInTheDocument();
    // Fresh rates -> no stale-rates indicator icon at all.
    expect(screen.queryByTestId("summary-rates-stale-icon")).not.toBeInTheDocument();
  });

  it("lists unconverted currencies in the context row", () => {
    wrap(<SummaryCards summary={makeSummary({ unconverted: ["KZT"] })} mode="native" />);

    expect(screen.getByText(/не учтены:.*KZT/)).toBeInTheDocument();
  });

  it("shows the rates date only in the stale-rates icon's tooltip, not as text", () => {
    wrap(<SummaryCards summary={makeSummary({ rates_on: "2026-07-20" })} mode="native" />);

    expect(screen.queryByText(/20\.07\.2026/)).not.toBeInTheDocument();
    const icon = screen.getByTestId("summary-rates-stale-icon");
    expect(icon).toHaveAttribute("title", RATES_ON);
  });

  // #109.3: with several currencies behind the total, the date belongs to
  // the furthest-back rate; «курс от …» named one rate that does not exist.
  it("says the named rate is the oldest of several, not the one rate behind the total", () => {
    wrap(
      <SummaryCards
        summary={makeSummary({
          totals: [
            { currency: "RUB", assets_minor: 100_000, liabilities_minor: 0, net_minor: 100_000 },
            { currency: "USD", assets_minor: 200_000, liabilities_minor: 0, net_minor: 200_000 },
            { currency: "EUR", assets_minor: 300_000, liabilities_minor: 0, net_minor: 300_000 },
          ],
          rates_on: "2026-07-20",
        })}
        mode="native"
      />,
    );

    const title = screen.getByTestId("summary-rates-stale-icon").getAttribute("title") ?? "";
    expect(title).toBe(RATES_ON);
    // Both claims, each on its own: a rate per currency, and the oldest date.
    expect(title).toContain("каждая по своему курсу");
    expect(title).toContain("самый старый");
  });

  it("hides the rates date and the stale-rates icon when rates are today or yesterday", () => {
    const today = localToday();
    wrap(<SummaryCards summary={makeSummary({ rates_on: today })} mode="native" />);
    expect(screen.queryByText(/курс/)).not.toBeInTheDocument();
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
    // #107: forty kopecks showed as a green «0 ₽» on the total card. Compared
    // with the zero rendering as a whole string.
    wrap(<SummaryCards summary={makeSummary({ total_in_base_minor: 40 })} mode="native" />);

    const amount = screen.getByTestId("summary-total-amount");
    expect(norm(amount.textContent ?? "")).toBe("0,40 ₽");
    expect(amount.textContent).not.toBe(formatMinorCompact(0, "RUB"));
    // The colour stays: this total is a gain.
    expect(amount.className).toContain("emerald");
  });

  it("omits the rates-date fragment when rates_on is unparseable", () => {
    wrap(
      <SummaryCards summary={makeSummary({ rates_on: "garbage" })} mode="native" />,
    );

    // No rates wording and no stale-rates icon without a valid date.
    expect(screen.queryByText(/курс/)).not.toBeInTheDocument();
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

  it("says what the total owes to journals, and only when there is something to say", () => {
    const { rerender } = wrap(<SummaryCards summary={makeSummary()} mode="native" />);
    expect(screen.queryByTestId("summary-journal-differing")).not.toBeInTheDocument();
    expect(screen.queryByTestId("summary-journal-pinned")).not.toBeInTheDocument();
    expect(screen.queryByTestId("summary-journal-unpriced")).not.toBeInTheDocument();

    rerender(
      <SummaryCards
        summary={makeSummary({
          journal: {
            accounts: 2,
            differing: 1,
            differing_difference_minor: -34_500_000,
            pinned_to_balance: 1,
            unpriced_positions: 3,
            not_traded_positions: 0,
            full_difference_minor: 0,
          },
        })}
        mode="native"
      />,
    );
    expect(norm(screen.getByTestId("summary-journal-differing").textContent ?? "")).toBe(
      norm(
        `не сходятся с балансом счетов: 1 — разница ${formatMinor(-34_500_000, "RUB")}, итог может быть неточным`,
      ),
    );
    expect(screen.getByTestId("summary-journal-pinned")).toHaveTextContent(
      "считаются по балансу, а не по журналу, счетов: 1",
    );
    expect(screen.getByTestId("summary-journal-unpriced")).toHaveTextContent(
      "бумаг без цены посчитано нулём: 3",
    );
  });
});

// Р-11: the total is the liquid worth; the full one is said beneath it, and
// the holdings it adds are told apart from those nothing prices.
describe("SummaryCards — the full valuation", () => {
  const journal = (overrides: Partial<Summary["journal"]>): Summary["journal"] => ({
    accounts: 1,
    differing: 0,
    differing_difference_minor: 0,
    pinned_to_balance: 0,
    unpriced_positions: 0,
    not_traded_positions: 0,
    full_difference_minor: 0,
    ...overrides,
  });

  it("says nothing when the two are one", () => {
    wrap(<SummaryCards summary={makeSummary()} mode="native" />);
    expect(screen.queryByTestId("summary-full-valuation")).toBeNull();
  });

  it("adds what does not sell now beneath the total", () => {
    wrap(
      <SummaryCards
        summary={makeSummary({ journal: journal({ full_difference_minor: 5_000_000, unpriced_positions: 3, not_traded_positions: 2 }) })}
        mode="native"
      />,
    );
    expect(norm(screen.getByTestId("summary-full-valuation").textContent ?? "")).toBe(
      norm(`полная оценка: ${formatMinorCompact(123_456_700 + 5_000_000, "RUB")}`),
    );
    expect(screen.getByTestId("summary-journal-not-traded").textContent).toBe(
      "не торгуются сейчас и в итог не вошли: 2",
    );
    expect(screen.getByTestId("summary-journal-unpriced").textContent).toBe("бумаг без цены посчитано нулём: 1");
  });
});
