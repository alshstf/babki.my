import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { RealizedTotal } from "./realized-total";
import type { RealizedTotal as RealizedTotalPayload } from "@/api/positions";

// NBSP-insensitive compare: Intl.NumberFormat uses non-breaking spaces
// (matches the helper in positions-table.test.tsx / money.test.ts).
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

// The account's total exactly as the server publishes it: both forms at once,
// because the response cannot know which one the toggle is on (see
// RealizedTotal in the API contract). This component adds nothing to it — every
// test here is about which of the server's figures reaches the screen and what
// is said when there is none.
function makeTotal(overrides: Partial<RealizedTotalPayload> = {}): RealizedTotalPayload {
  return {
    by_currency: [{ currency: "USD", realized_pnl_minor: 12_500 }],
    base_currency: "RUB",
    in_base: 1_000_000,
    in_base_gap: null,
    ...overrides,
  };
}

describe("RealizedTotal", () => {
  it("shows the figure under its own label", () => {
    render(<RealizedTotal total={makeTotal()} mode="native" />);

    expect(screen.getByText("Зафиксировано")).toBeInTheDocument();
    expect(norm(screen.getByTestId("realized-total-amounts").textContent ?? "")).toContain(
      "125,00 $",
    );
  });

  it("explains in a tooltip how this differs from the profit on paper", () => {
    // Which rates stand behind the two ends of the figure is exactly the
    // detail that belongs in a tooltip rather than in the text of a screen
    // full of numbers (the owner's standing rule).
    render(<RealizedTotal total={makeTotal()} mode="native" />);

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
        total={makeTotal({
          by_currency: [
            { currency: "EUR", realized_pnl_minor: 2_500 },
            { currency: "USD", realized_pnl_minor: 10_000 },
          ],
        })}
        mode="native"
      />,
    );

    const shown = norm(screen.getByTestId("realized-total-amounts").textContent ?? "");
    expect(shown).toContain("25,00 €");
    expect(shown).toContain("100,00 $");
  });

  it("shows the base-currency figure, not the position-currency ones, in base mode", () => {
    // The two are different numbers on purpose: the base figure carries the
    // currency's move between purchase and sale (see PositionInBase in the API
    // contract), so showing the wrong one is a silently wrong total.
    render(
      <RealizedTotal
        total={makeTotal({
          by_currency: [{ currency: "USD", realized_pnl_minor: 12_500 }],
          in_base: 900_000,
        })}
        mode="base"
      />,
    );

    const shown = norm(screen.getByTestId("realized-total-amounts").textContent ?? "");
    expect(shown).toContain("9 000,00 ₽");
    expect(shown).not.toContain("125,00 $");
  });

  it("shows no base-currency sum, and says what about the deals stopped it", () => {
    render(
      <RealizedTotal total={makeTotal({ in_base: null, in_base_gap: "undated" })} mode="base" />,
    );

    expect(screen.queryByTestId("realized-total-amounts")).not.toBeInTheDocument();
    const gap = screen.getByTestId("realized-total-gap");
    // A fact about the reader's own deals, and one that says of itself that it
    // will not fix itself — not the name of a field that was left blank.
    expect(gap.textContent).toContain("когда была куплена");
    expect(gap.textContent).toContain("неоткуда");
    expect(gap.textContent).not.toContain("дат");
    // Saying "нет курса" here would name a cause that will never be the true
    // one and promise a number that is never coming.
    expect(gap.textContent).not.toContain("курс");
    expect(gap.getAttribute("title")).toContain("никогда");
  });

  it("says the rate is what stopped it when nothing about the deals is unknown", () => {
    render(
      <RealizedTotal total={makeTotal({ in_base: null, in_base_gap: "no_rate" })} mode="base" />,
    );

    const gap = screen.getByTestId("realized-total-gap");
    expect(gap.textContent).toContain("курс");
    expect(gap.textContent).not.toContain("когда была куплена");
    // A rate that has not been fetched yet is a gap that closes on its own.
    expect(gap.getAttribute("title")).toContain("обнов");
  });

  it("names both causes when different positions were stopped by different gaps", () => {
    render(<RealizedTotal total={makeTotal({ in_base: null, in_base_gap: "both" })} mode="base" />);

    const gap = screen.getByTestId("realized-total-gap");
    expect(gap.textContent).toContain("когда была куплена");
    expect(gap.textContent).toContain("курс");
  });

  it("keeps showing the per-currency figures when only the converted sum is missing", () => {
    // A gap is a fact about the base-currency sum alone. In the positions' own
    // currency every figure is published unconditionally, and withholding a
    // complete answer because another one is incomplete is the silence this
    // screen exists to remove.
    render(
      <RealizedTotal
        total={makeTotal({ in_base: null, in_base_gap: "undated" })}
        mode="native"
      />,
    );

    expect(screen.queryByTestId("realized-total-gap")).not.toBeInTheDocument();
    expect(norm(screen.getByTestId("realized-total-amounts").textContent ?? "")).toContain(
      "125,00 $",
    );
  });

  it("renders nothing when the account has no positions at all", () => {
    // by_currency is empty exactly then, and a "0,00" over an empty account
    // answers a question nobody asked. The base figure is a real zero here —
    // the sum of no deals — and it must not be what decides.
    render(
      <RealizedTotal total={makeTotal({ by_currency: [], in_base: 0 })} mode="base" />,
    );

    expect(screen.queryByTestId("realized-total")).not.toBeInTheDocument();
  });

  it("renders nothing rather than a reason of its own when the server names none", () => {
    // The contract publishes a figure or a gap, never neither. If that ever
    // breaks, an unexplained blank is honest and a cause guessed here is not.
    render(
      <RealizedTotal total={makeTotal({ in_base: null, in_base_gap: null })} mode="base" />,
    );

    expect(screen.queryByTestId("realized-total")).not.toBeInTheDocument();
  });
});
