import { describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import "@/i18n";
import { RealizedTotal } from "./realized-total";
import type { RealizedTotal as RealizedTotalPayload } from "@/api/positions";

// NBSP-insensitive compare.
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

// The account's total as published, both forms: the response does not
// know the toggle. The component adds nothing to it.
function makeTotal(
  overrides: Partial<RealizedTotalPayload> = {},
): RealizedTotalPayload {
  return {
    by_currency: [{ currency: "USD", realized_pnl_minor: 12_500 }],
    base_currency: "RUB",
    tax_withheld_by_currency: [],
    in_base: 1_000_000,
    in_base_gap: null,
    undated_positions: 0,
    unknown_cost_positions: 0,
    ...overrides,
  };
}

// The label's tooltip, pinned whole: its rate claims mirror
// internal/portfolio/rates.go's realizedTerms, so a rewording must come
// past this test. #109.1: a disposal's fee is valued on the sale day; only
// the retired basis uses purchase days.
const REALIZED_HINT =
  "Результат уже закрытых сделок по этому счёту. От цен рынка он не зависит, а меняется, только если меняется сама история операций. Стоимость проданного взята по курсам на дни расчётов по покупкам, а выручка и комиссия продажи — по курсу на день расчётов по продаже (если день расчётов неизвестен — на день сделки), поэтому в базовой валюте сюда входит и изменение курса. Это только сделки: выплаты по бумагам сюда не входят, они складываются с этой суммой в колонке «Зафиксировано»";

describe("RealizedTotal", () => {
  it("shows the figure under its own label", () => {
    render(<RealizedTotal total={makeTotal()} mode="native" />);

    expect(screen.getByText("Реализованная прибыль")).toBeInTheDocument();
    expect(
      norm(screen.getByTestId("realized-total-amounts").textContent ?? ""),
    ).toContain("125,00 $");
  });

  it("explains in a tooltip how this differs from the profit on paper", () => {
    // Which rates stand behind the figure belongs in the tooltip.
    render(<RealizedTotal total={makeTotal()} mode="native" />);

    const hint =
      screen.getByTestId("realized-total-label").getAttribute("title") ?? "";
    expect(hint).toContain("на дни расчётов по покупкам");
    expect(hint).toContain("на день расчётов по продаже");
    expect(hint).toContain("если день расчётов неизвестен — на день сделки");
    expect(hint).toContain("изменение курса");
    // A settlement day learned later restates the figure, so the hint does not
    // promise it is final.
    expect(hint).not.toContain("не изменится");
    // The label is not the table's «Зафиксировано», which adds the paper's
    // payments: one word would name two numbers on one screen.
    expect(screen.getByTestId("realized-total-label").textContent).toBe(
      "Реализованная прибыль",
    );
  });

  it("dates a sale's fee on the sale day, as the server does, not on the purchase days", () => {
    render(<RealizedTotal total={makeTotal()} mode="native" />);

    expect(
      screen.getByTestId("realized-total-label").getAttribute("title"),
    ).toBe(REALIZED_HINT);
    // Both halves of the expense clause are asserted: fee on the sale day,
    // basis on the purchase days.
    expect(REALIZED_HINT).toContain(
      "комиссия продажи — по курсу на день расчётов по продаже",
    );
    expect(REALIZED_HINT).toContain(
      "Стоимость проданного взята по курсам на дни расчётов по покупкам",
    );
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

    const shown = norm(
      screen.getByTestId("realized-total-amounts").textContent ?? "",
    );
    expect(shown).toContain("25,00 €");
    expect(shown).toContain("100,00 $");
  });

  it("shows the base-currency figure, not the position-currency ones, in base mode", () => {
    // Different numbers on purpose: the base figure carries the currency's
    // move between purchase and sale.
    render(
      <RealizedTotal
        total={makeTotal({
          by_currency: [{ currency: "USD", realized_pnl_minor: 12_500 }],
          in_base: 900_000,
        })}
        mode="base"
      />,
    );

    const shown = norm(
      screen.getByTestId("realized-total-amounts").textContent ?? "",
    );
    expect(shown).toContain("9 000,00 ₽");
    expect(shown).not.toContain("125,00 $");
  });

  // #195: a parcel sold without a purchase day can never be valued; the
  // server leaves such positions out and counts them.
  it("shows the sum of what can be valued and counts what was left out", () => {
    render(
      <RealizedTotal
        total={makeTotal({ in_base: 450_000, undated_positions: 2 })}
        mode="base"
      />,
    );

    expect(
      norm(screen.getByTestId("realized-total-amounts").textContent ?? ""),
    ).toContain("4 500,00 ₽");
    expect(screen.queryByTestId("realized-total-gap")).not.toBeInTheDocument();
    const note = screen.getByTestId("realized-total-undated");
    expect(note.textContent).toContain("2");
    // A fact about the reader's deals that will not fix itself; "нет курса"
    // would name a false cause.
    expect(note.textContent).toContain("когда куплено");
    expect(note.textContent).not.toContain("курс");
    expect(note.getAttribute("title")).toContain("не появится");
  });

  // A sale of shares with no purchase price, counted as bought for nothing:
  // said in both modes, never instead of the figure.
  it("says in both modes that some sales were counted as bought for nothing", () => {
    for (const mode of ["base", "native"] as const) {
      cleanup();
      render(
        <RealizedTotal
          total={makeTotal({ unknown_cost_positions: 1 })}
          mode={mode}
        />,
      );
      expect(screen.getByTestId("realized-total-amounts")).toBeInTheDocument();
      const note = screen.getByTestId("realized-total-unknown-cost");
      expect(note.textContent).toContain("купленными за 0");
      expect(note.getAttribute("title")).toContain("завышена");
    }
  });

  it("says nothing about left-out positions when there are none", () => {
    render(<RealizedTotal total={makeTotal()} mode="base" />);
    expect(
      screen.queryByTestId("realized-total-undated"),
    ).not.toBeInTheDocument();
  });

  it("says the rate is what stopped it when nothing about the deals is unknown", () => {
    render(
      <RealizedTotal
        total={makeTotal({ in_base: null, in_base_gap: "no_rate" })}
        mode="base"
      />,
    );

    const gap = screen.getByTestId("realized-total-gap");
    expect(gap.textContent).toContain("курс");
    expect(gap.textContent).not.toContain("когда была куплена");
    // A rate that has not been fetched yet is a gap that closes on its own.
    expect(gap.getAttribute("title")).toContain("обнов");
  });

  it("waits for the rate and still counts what will never be valued", () => {
    render(
      <RealizedTotal
        total={makeTotal({
          in_base: null,
          in_base_gap: "no_rate",
          undated_positions: 1,
        })}
        mode="base"
      />,
    );

    expect(screen.getByTestId("realized-total-gap").textContent).toContain(
      "курс",
    );
    expect(screen.getByTestId("realized-total-undated")).toBeInTheDocument();
  });

  it("keeps showing the per-currency figures when only the converted sum is missing", () => {
    // A gap is about the base sum alone; the native figures are always
    // published and shown.
    render(
      <RealizedTotal
        total={makeTotal({
          in_base: null,
          in_base_gap: "no_rate",
          undated_positions: 1,
        })}
        mode="native"
      />,
    );

    expect(screen.queryByTestId("realized-total-gap")).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("realized-total-undated"),
    ).not.toBeInTheDocument();
    expect(
      norm(screen.getByTestId("realized-total-amounts").textContent ?? ""),
    ).toContain("125,00 $");
  });

  // The tax the account was charged: a Russian broker withholds against
  // the year's base when money leaves, so it belongs to no paper.
  it("shows what the broker withheld from the account, beside the result it was charged against", () => {
    render(
      <RealizedTotal
        total={makeTotal({
          tax_withheld_by_currency: [
            { currency: "RUB", amount_minor: 3_600_000 },
          ],
        })}
        mode="native"
      />,
    );

    const tax = screen.getByTestId("realized-total-tax");
    expect(norm(tax.textContent ?? "")).toBe("удержано налога 36 000,00 ₽");
    expect(tax.getAttribute("title")).toBe(
      "Налог, который брокер списал со счёта, а не с выплаты по бумаге: в России — при выводе средств, с накопленной за год базы. Поэтому он не относится ни к одной позиции и по строкам не разносится. Налог, удержанный с дивиденда или купона, в эту сумму не входит — он уже вычтен из дохода той бумаги",
    );
  });

  it("keeps the withheld tax in its own currency even in base mode", () => {
    // Shown unconverted: the response has no per-charge dates to convert by.
    render(
      <RealizedTotal
        total={makeTotal({
          tax_withheld_by_currency: [{ currency: "USD", amount_minor: 1_000 }],
        })}
        mode="base"
      />,
    );

    expect(
      norm(screen.getByTestId("realized-total-tax").textContent ?? ""),
    ).toBe("удержано налога 10,00 $");
    // ...while the realized figure beside it IS the base-currency one.
    expect(
      norm(screen.getByTestId("realized-total-amounts").textContent ?? ""),
    ).toContain("10 000,00 ₽");
  });

  it("lists two currencies side by side and drops a bucket that is nought", () => {
    // A zero bucket is published but dropped here: "0,00 $" beside a real
    // charge reads as a second one.
    render(
      <RealizedTotal
        total={makeTotal({
          tax_withheld_by_currency: [
            { currency: "RUB", amount_minor: 3_600_000 },
            { currency: "USD", amount_minor: 0 },
          ],
        })}
        mode="native"
      />,
    );

    const tax = norm(
      screen.getByTestId("realized-total-tax").textContent ?? "",
    );
    expect(tax).toBe("удержано налога 36 000,00 ₽");
    expect(tax).not.toContain("$");
  });

  it("shows a withholding on an account that has closed no deals at all", () => {
    // A dividend tax recorded as its own operation charges the account even
    // with every position open: the line appears for the tax alone.
    render(
      <RealizedTotal
        total={makeTotal({
          by_currency: [],
          in_base: 0,
          tax_withheld_by_currency: [
            { currency: "RUB", amount_minor: 130_000 },
          ],
        })}
        mode="base"
      />,
    );

    expect(
      norm(screen.getByTestId("realized-total-tax").textContent ?? ""),
    ).toBe("удержано налога 1 300,00 ₽");
    expect(screen.getByTestId("realized-total-amounts").textContent).toBe("");
  });

  it("renders nothing when the account has no positions at all", () => {
    // by_currency is empty then; the base figure is a real zero and must not
    // decide.
    render(
      <RealizedTotal
        total={makeTotal({ by_currency: [], in_base: 0 })}
        mode="base"
      />,
    );

    expect(screen.queryByTestId("realized-total")).not.toBeInTheDocument();
  });

  it("renders nothing rather than a reason of its own when the server names none", () => {
    // The contract publishes a figure or a gap. If neither, a blank is
    // honest and a guessed cause is not.
    render(
      <RealizedTotal
        total={makeTotal({ in_base: null, in_base_gap: null })}
        mode="base"
      />,
    );

    expect(screen.queryByTestId("realized-total")).not.toBeInTheDocument();
  });

  it("renders nothing, not the label over an empty amount, when the wire names a gap kind this build cannot word", () => {
    // A gap value newer than this bundle passes the API boundary unchecked;
    // the cast stands in for it.
    const unknownGap =
      "future_gap_kind" as unknown as RealizedTotalPayload["in_base_gap"];
    render(
      <RealizedTotal
        total={makeTotal({ in_base: null, in_base_gap: unknownGap })}
        mode="base"
      />,
    );

    expect(screen.queryByTestId("realized-total")).not.toBeInTheDocument();
    expect(screen.queryByTestId("realized-total-gap")).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("realized-total-amounts"),
    ).not.toBeInTheDocument();
  });
});

// Одна из позиций продана в другой валюте: итога в одной валюте нет, и
// ноль на его месте нельзя было бы отличить от настоящего.
describe("корзина без итога в одной валюте", () => {
  it("не рисуется, а остальные валюты остаются на месте", () => {
    render(
      <RealizedTotal
        total={makeTotal({
          by_currency: [
            { currency: "CNY", realized_pnl_minor: null },
            { currency: "USD", realized_pnl_minor: 12_500 },
          ],
        })}
        mode="native"
      />,
    );
    const amounts = screen.getByTestId("realized-total-amounts");
    expect(amounts).toHaveTextContent("125,00");
    expect(amounts).not.toHaveTextContent("CNY");
    expect(amounts).not.toHaveTextContent("0,00");
  });

  it("не оставляет пустую строку, когда таких корзин все", () => {
    const { container } = render(
      <RealizedTotal
        total={makeTotal({
          by_currency: [{ currency: "CNY", realized_pnl_minor: null }],
        })}
        mode="native"
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});
