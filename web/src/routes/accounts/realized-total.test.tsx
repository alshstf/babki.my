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
function makeTotal(
  overrides: Partial<RealizedTotalPayload> = {},
): RealizedTotalPayload {
  return {
    by_currency: [{ currency: "USD", realized_pnl_minor: 12_500 }],
    base_currency: "RUB",
    tax_withheld_by_currency: [],
    in_base: 1_000_000,
    in_base_gap: null,
    ...overrides,
  };
}

// The label's tooltip, pinned as one exact string. What it says about the
// rates is a claim about internal/portfolio/http.go's realizedTerms, and the
// only way a wrong claim shows up is by reading the sentence against that
// function — so the sentence lives here in full rather than being sampled by
// substring, and a rewording has to come past this test.
//
// #109.1 is the clause about the FEE. realizedTerms emits a disposal's fee as
// {minor: -e.FeeMinor, on: e.OccurredOn} — the day of the disposal, the same
// day as its proceeds — while the purchase dates value the retired basis and
// nothing else. The caption used to put every expense «по курсам на дни
// покупок», which is false of a commission the broker charged on the day of
// the sale.
const REALIZED_HINT =
  "Результат уже закрытых сделок по этому счёту — он больше не изменится. Стоимость проданного взята по курсам на дни покупок, а выручка и комиссия продажи — по курсу на день продажи, поэтому в базовой валюте сюда входит и изменение курса. Это только сделки: выплаты по бумагам сюда не входят, они складываются с этой суммой в колонке «Зафиксировано»";

describe("RealizedTotal", () => {
  it("shows the figure under its own label", () => {
    render(<RealizedTotal total={makeTotal()} mode="native" />);

    expect(screen.getByText("Реализованная прибыль")).toBeInTheDocument();
    expect(
      norm(screen.getByTestId("realized-total-amounts").textContent ?? ""),
    ).toContain("125,00 $");
  });

  it("explains in a tooltip how this differs from the profit on paper", () => {
    // Which rates stand behind the two ends of the figure is exactly the
    // detail that belongs in a tooltip rather than in the text of a screen
    // full of numbers (the owner's standing rule).
    render(<RealizedTotal total={makeTotal()} mode="native" />);

    const hint =
      screen.getByTestId("realized-total-label").getAttribute("title") ?? "";
    expect(hint).toContain("на дни покупок");
    expect(hint).toContain("на день продажи");
    expect(hint).toContain("изменение курса");
    // The label itself stays a label; the mechanics are not printed as text.
    // And it is NOT the table's word: «Зафиксировано» there adds the payments
    // the paper made to this figure, so one word over both would name two
    // different numbers on the same screen.
    expect(screen.getByTestId("realized-total-label").textContent).toBe(
      "Реализованная прибыль",
    );
  });

  it("dates a sale's fee on the sale day, as the server does, not on the purchase days", () => {
    render(<RealizedTotal total={makeTotal()} mode="native" />);

    expect(
      screen.getByTestId("realized-total-label").getAttribute("title"),
    ).toBe(REALIZED_HINT);
    // The specific thing that was false: an expense clause that swept the fee
    // in with the basis. Both halves are asserted, so neither "the fee moved
    // to the sale day" nor "the basis stayed on the purchase days" can be
    // dropped without this failing.
    expect(REALIZED_HINT).toContain(
      "комиссия продажи — по курсу на день продажи",
    );
    expect(REALIZED_HINT).toContain(
      "Стоимость проданного взята по курсам на дни покупок",
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

    const shown = norm(
      screen.getByTestId("realized-total-amounts").textContent ?? "",
    );
    expect(shown).toContain("9 000,00 ₽");
    expect(shown).not.toContain("125,00 $");
  });

  it("shows no base-currency sum, and says what about the deals stopped it", () => {
    render(
      <RealizedTotal
        total={makeTotal({ in_base: null, in_base_gap: "undated" })}
        mode="base"
      />,
    );

    expect(
      screen.queryByTestId("realized-total-amounts"),
    ).not.toBeInTheDocument();
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

  it("names both causes when different positions were stopped by different gaps", () => {
    render(
      <RealizedTotal
        total={makeTotal({ in_base: null, in_base_gap: "both" })}
        mode="base"
      />,
    );

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
    expect(
      norm(screen.getByTestId("realized-total-amounts").textContent ?? ""),
    ).toContain("125,00 $");
  });

  // THE TAX THE ACCOUNT ITSELF WAS CHARGED. In Russia the broker withholds at
  // the moment money leaves the account, against the year's accumulated base —
  // so the charge belongs to no paper and cannot be spread over the rows. The
  // owner met it as 36 000 ₽ he could not attribute to anything; these tests
  // are about that figure having a place on the screen and an honest sentence.
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
    // Everything else on this line converts; this does not, and the difference
    // is not an oversight. A withholding is money taken on a day, and the
    // response carries no per-charge dates to convert it by — so it is shown as
    // what it is rather than as a figure struck at a rate nobody chose.
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
    // Nought withheld is not news, and a "0,00 $" beside a real charge reads as
    // a second charge. The server publishes the bucket all the same — it is the
    // sum of the operations it found — so the dropping happens here.
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
    // The case that has nothing to do with sales: a broker that records the tax
    // on a dividend as its own operation with no paper attached charges the
    // ACCOUNT, and an account whose every position is still open then has a
    // withholding and no realized result anywhere. The line must appear for the
    // tax alone — and say nothing about a realized total it does not have.
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
    // by_currency is empty exactly then, and a "0,00" over an empty account
    // answers a question nobody asked. The base figure is a real zero here —
    // the sum of no deals — and it must not be what decides.
    render(
      <RealizedTotal
        total={makeTotal({ by_currency: [], in_base: 0 })}
        mode="base"
      />,
    );

    expect(screen.queryByTestId("realized-total")).not.toBeInTheDocument();
  });

  it("renders nothing rather than a reason of its own when the server names none", () => {
    // The contract publishes a figure or a gap, never neither. If that ever
    // breaks, an unexplained blank is honest and a cause guessed here is not.
    render(
      <RealizedTotal
        total={makeTotal({ in_base: null, in_base_gap: null })}
        mode="base"
      />,
    );

    expect(screen.queryByTestId("realized-total")).not.toBeInTheDocument();
  });

  it("renders nothing, not the label over an empty amount, when the wire names a gap kind this build cannot word", () => {
    // A client can run slightly behind the server it talks to: RealizedGap
    // grows a member the bundle in the browser was built before. That value
    // is still valid JSON and still passes through the type assertion at the
    // API boundary unchanged — TypeScript's exhaustiveness check on
    // gapWording's switch cannot see it, because it never saw the string at
    // compile time. The cast below stands in for exactly that value.
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

// Одна из позиций продана в другой валюте: у корзины этой валюты нет итога
// в одной валюте вообще, и строка не имеет права нарисовать вместо него ноль.
// Ноль — это тоже настоящий результат, и два случая стали бы неразличимы.
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
