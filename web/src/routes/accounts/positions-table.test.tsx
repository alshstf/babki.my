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
// (matches the helper in money.test.ts / summary-cards.test.tsx). Written
// with explicit escapes (rather than the literal characters) so they can't
// silently get mangled into plain ASCII spaces by an editing tool.
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
        "Нет курса — показано в исходной валюте",
      );
      expect(screen.getByTestId("position-market-value-not-converted")).toBeInTheDocument();
      expect(screen.getByTestId("position-profit-amount-not-converted")).toBeInTheDocument();
      expect(screen.getByTestId("position-income-not-converted")).toBeInTheDocument();
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

    it("names the source currency, not the account's, in the not-converted marker", () => {
      // A position row's amounts are in the position's / quote's / bond face
      // value's currency — calling that "the account's currency" would point
      // the user at the wrong thing.
      wrap(
        <PositionsTable
          positions={[makePosition({ currency: "USD", in_base: null })]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      expect(screen.getByTestId("position-cost-not-converted")).toHaveAttribute(
        "title",
        "Нет курса — показано в исходной валюте",
      );
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
              in_base: {
                cost_minor: 9_000_000,
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

      // Cost converted, as usual.
      expect(norm(screen.getByTestId("position-cost").textContent ?? "")).toBe(
        norm(formatMinor(9_000_000, "RUB")),
      );
      // The valuation stays the real 1 000,00 € with a marker — never
      // 90 000,00 ₽ (that same figure times the USD rate) passed off as base
      // currency, and never a dash or a zero.
      const marketValue = screen.getByTestId("position-market-value");
      expect(norm(marketValue.textContent ?? "")).toContain(norm(formatMinor(100_000, "EUR")));
      expect(marketValue.textContent).not.toMatch(/₽/);
      expect(screen.getByTestId("position-market-value-not-converted")).toHaveAttribute(
        "title",
        "Нет курса — показано в исходной валюте",
      );
      // Profit is derived from that valuation, so it stays an honest dash.
      expect(screen.getByTestId("position-profit-dash")).toHaveTextContent("—");
      expect(screen.queryByTestId("position-profit-amount")).not.toBeInTheDocument();
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
