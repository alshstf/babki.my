import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import "@/i18n";
import { PositionsTable } from "./positions-table";
import type { CashPosition, Position } from "@/api/positions";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { announcedText, visibleText } from "@/test-utils";

// PositionsTable is a pure presentational component (positions come in as a
// prop), so a bare render is enough — no QueryClientProvider needed.
function wrap(ui: ReactElement) {
  return render(ui);
}

// NBSP-insensitive compare (Intl uses non-breaking spaces), written with
// escapes so an editor cannot turn them into plain spaces.
const norm = (s: string) => s.replace(/[\u00A0\u202F]/g, " ");

// The row caption's sentences, one per Position.in_base_gap value plus the
// general fallback, spelled out rather than read from ru.json, so a test checks
// which sentence lands (#66), not that the lookup agrees with itself.
const CAPTION = {
  // The fallback for a cause this build cannot name claims only that the
  // base-currency figures were withheld; «Нет курса» would be false of
  // date-shaped causes (#105).
  general:
    "В базовой валюте эта позиция не посчиталась, а причина не названа. Поэтому числа этой строки показаны в исходной валюте",
  // Scoped to this row: another row may show cost in roubles and its
  // valuation in euros.
  undatedLot:
    "У одной из партий не записана дата покупки, а стоимость считается по курсу на день покупки — и восстановить эту дату уже неоткуда: в базовой валюте эта позиция сама не посчитается. Поэтому ни одно число этой строки не показано в базовой валюте",
  noRateLotDate:
    "Нет курса на день покупки одной из партий, а стоимость считается по курсу того дня. Если курс появится при обновлении курсов, позиция посчитается сама. Поэтому пока ни одно число этой строки не показано в базовой валюте",
  // These two say only that an earlier term did not stop the sum, not that
  // purchase-day rates were found: a position with no lots asks for none.
  noRateIncomeDate:
    "Дело не в стоимости: нет курса на день одной из выплат — дивиденда, купона или налога, — а доход считается по курсу того дня. Если курс появится при обновлении курсов, позиция посчитается сама. Поэтому пока ни одно число этой строки не показано в базовой валюте",
  noRateToday:
    "Дело не в стоимости и не в доходе: нет курса на сегодня — для валюты, в которой считается рыночная оценка, — а оценка берётся по текущему курсу. Если курс появится при обновлении курсов, позиция посчитается сама. Поэтому пока ни одно число этой строки не показано в базовой валюте",
  valuationCurrency:
    "Оценка получилась в другой валюте, чем позиция, а курса от неё до валюты позиции нет: сравнить её со стоимостью позиции нельзя. Пока оценка не выражена в валюте позиции, программа не показывает её и в базовой. Поэтому показана в исходной валюте",
} as const;

// The sentences for an empty valuation cell, one per no-valuation
// market_value_gap plus the general one (#78), spelled out like CAPTION. Only
// no_quote mentions a quote; the other two may not claim a quote exists or is
// missing, since the server reports them either way.
const NO_VALUATION = {
  noQuote:
    "Котировки пока нет. Если она появится, рыночная оценка посчитается сама",
  typeNotPriced:
    "Рыночную оценку для этого вида активов программа не считает — такого расчёта в ней нет. Котировка тут ничего не меняет: даже когда она есть, оценки не будет, и ждать её не нужно",
  noFaceValue:
    "У этой облигации не записан номинал, а котируется она в процентах от номинала: брать процент не от чего. Котировка тут ничего не меняет — пока номинала нет, оценки не будет",
  general: "Рыночной оценки нет, а причина не названа",
} as const;

// What the profit dash adds before the valuation's sentence.
const PROFIT_NEEDS_VALUATION =
  "Прибыль — это рыночная оценка минус стоимость, а оценки нет";

// The price tooltip's two sentences under the date, spelled out. price_on is the
// source's session, not the fetch day (#90); the caption says what the date is
// without claiming freshness, and avoids what live ISS measurements forbid (see
// QuotesFor in moex.go): not the closing price (PREVLEGALCLOSEPRICE differs:
// 276.52 vs 275.60 on SBER), not necessarily a traded price (779 of TQCB's 3021
// rows did not trade), not "previous session", not "fresh".
const PRICE_SESSION_NOTE =
  "Это цена той торговой сессии: так её датирует источник котировки, а не программа в день загрузки. Сделки в тот день могло и не быть — источник называет цену и для бумаги, которая не торговалась";
// The valuation is struck from this price and converted at the current rate
// (position_api.go), conditionally, since a native-currency row converts
// nothing.
const PRICE_VALUATION_NOTE =
  "Рыночная оценка посчитана из этой цены. Если оценку пересчитывают в другую валюту, берётся текущий курс, а не курс на эту дату";

// Every money cell carries the row's caption; the valuation may carry its
// own (market_value_gap).
const ROW_MARKERS = [
  "position-cost-not-converted",
  "position-market-value-not-converted",
  "position-profit-amount-not-converted",
  "position-settled-not-converted",
] as const;

// daysAgo spells a date relative to the clock, for the price-age rule; a
// literal would cross the threshold on its own.
function daysAgo(n: number): string {
  return new Date(Date.now() - n * 86_400_000).toISOString().slice(0, 10);
}

// makePosition builds a row as the server would, deriving settled (realized +
// income) and total (+ unrealized) unless a test overrides them, which is how
// the null cases are written.
function makePosition(overrides: Partial<Position> = {}): Position {
  const base: Position = {
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
    settled_minor: 0,
    total_minor: 0,
    income_minor: 0,
    // Nothing paid, so the list is empty; a fixture with income in another
    // currency sets both fields consistently (income_minor is this list's entry
    // for `currency`).
    income_by_currency: [],
    fees_minor: 0,
    currency: "USD",
    market_value_minor: 305_50,
    market_value_currency: "USD",
    // The ordinary case: the market price is both valuations (decision Р-11).
    liquid_value_minor: 305_50,
    price_source: "market",
    price: "305.5",
    // More than a month old on purpose: these rows show «цена не
    // обновлялась». Fresh-price tests pass their own date.
    price_on: "2026-07-20",
    // 2500,00 cost, +250,00 unrealized -> +10,0 %, so the profit column is
    // exercised by default.
    unrealized_pnl_minor: 25_000,
    // The ordinary case: every lot came from a buy, and a buy always knows
    // its own date. Tests about the other case set it explicitly.
    has_undated_lots: false,
    // Not read by PositionsTable; kept at its honest default.
    has_undated_realizations: false,
    has_unknown_cost: false,
    // The server's two named causes (#66), null here since the row converts.
    // A test that nulls in_base sets in_base_gap too, as the server does.
    in_base_gap: null,
    market_value_gap: null,
    ...overrides,
  };
  const settled =
    "settled_minor" in overrides
      ? base.settled_minor
      : base.realized_pnl_minor == null
        ? null
        : base.realized_pnl_minor + base.income_minor;
  const total =
    "total_minor" in overrides
      ? base.total_minor
      : settled == null || base.unrealized_pnl_minor == null
        ? null
        : settled + base.unrealized_pnl_minor;
  return { ...base, settled_minor: settled, total_minor: total };
}

describe("PositionsTable", () => {
  it("shows the market value amount and price, with the quote date only in a tooltip", () => {
    // A fresh price; the fixture's own is deliberately old.
    wrap(
      <PositionsTable
        positions={[makePosition({ price_on: daysAgo(2) })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(
      norm(screen.getByTestId("position-market-value").textContent ?? ""),
    ).toBe(norm(formatMinor(305_50, "USD")));
    // Price is shown as text...
    const priceLine = screen.getByTestId("position-price");
    expect(norm(priceLine.textContent ?? "")).toBe("305,50 $");
    // The date moved into the tooltip, read from the fixture's day.
    const shown = formatDate(daysAgo(2));
    expect(
      screen.queryByText(new RegExp(shown.replace(/\./g, "\\."))),
    ).not.toBeInTheDocument();
    expect(priceLine.getAttribute("title")).toContain(`Цена на ${shown}`);
  });

  it("captions the price with the session it belongs to, not with the day it was fetched", () => {
    wrap(
      <PositionsTable
        positions={[makePosition()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    // One exact string, in order, so any swapped, dropped or reordered
    // sentence fails.
    expect(screen.getByTestId("position-price").getAttribute("title")).toBe(
      `Цена на 20.07.2026\n${PRICE_SESSION_NOTE}\n${PRICE_VALUATION_NOTE}`,
    );
  });

  it("prints every converted figure of a row in the currency that row's in_base carries, not the session's", () => {
    // #106: after a base-currency change the session updates at once while
    // cached figures are still in the old currency. All four cells must keep
    // the old currency's sign, each reading its own block.
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            currency: "USD",
            income_minor: 1_000,
            in_base: {
              cost_minor: 2_275_000,
              market_value_minor: 2_780_050,
              settled_minor: 9_100,
              total_minor: 236_600,
              unrealized_pnl_minor: 227_500,
              income_minor: 9_100,
              currency: "RUB",
              rate_on: "2026-07-22",
            },
          }),
        ]}
        mode="base"
        baseCurrency="EUR"
      />,
    );

    for (const [testId, minor] of [
      ["position-cost", 2_275_000],
      ["position-market-value", 2_780_050],
      ["position-profit-amount", 227_500],
      ["position-settled", 9_100],
    ] as const) {
      const cell = screen.getByTestId(testId);
      expect(norm(cell.textContent ?? "")).toBe(
        norm(formatMinor(minor, "RUB")),
      );
      expect(cell.textContent).not.toContain("€");
    }
  });

  it("dates the price by price_on and the conversion by rate_on — two fields, two dates, one format", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            currency: "USD",
            in_base: {
              cost_minor: 2_275_000,
              market_value_minor: 2_780_050,
              settled_minor: null,
              total_minor: null,
              unrealized_pnl_minor: 227_500,
              income_minor: 0,
              currency: "RUB",
              // Not the quote's date: the two dates answer different questions.
              rate_on: "2026-07-22",
            },
          }),
        ]}
        mode="base"
        baseCurrency="RUB"
      />,
    );

    const priceTitle =
      screen.getByTestId("position-price").getAttribute("title") ?? "";
    expect(priceTitle).toContain("Цена на 20.07.2026");
    expect(priceTitle).not.toContain("22.07.2026");
    // The neighbour's date, rendered by the same formatDate: same dd.MM.yyyy
    // shape, so the two dates on one row cannot be read as two conventions.
    expect(
      screen.getByTestId("position-market-value").getAttribute("title"),
    ).toBe("Пересчитано по текущему курсу (на 22.07.2026)");
  });

  it("shows an honest dash with a tooltip instead of a fake zero when there is no quote", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            // Non-zero income so the "no fake 0,00" check depends on the market
            // value column.
            income_minor: 500,
            market_value_minor: null,
            market_value_currency: null,
            price: null,
            price_on: null,
            // no_quote: the one case «нет котировки» is true of.
            market_value_gap: "no_quote",
          }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const dash = screen.getByTestId("position-no-quote");
    expect(dash).toHaveTextContent("—");
    expect(dash).toHaveAttribute("title", NO_VALUATION.noQuote);
    // Excludes amounts merely ending in "0,00" ("500,00").
    expect(screen.queryByText(/(?<!\d)0,00/)).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("position-market-value"),
    ).not.toBeInTheDocument();
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
    expect(norm(amount.textContent ?? "")).toBe(
      norm(formatMinor(100_000, "EUR")),
    );
    expect(amount.textContent).not.toMatch(/₽/);
  });

  it("does not render the removed realized/fees columns", () => {
    wrap(
      <PositionsTable
        positions={[makePosition()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.queryByText("Реализовано")).not.toBeInTheDocument();
    expect(screen.queryByText("Комиссии")).not.toBeInTheDocument();
    expect(screen.getByText("Прибыль")).toBeInTheDocument();
  });

  it("shows unrealized profit with its percentage of cost", () => {
    // cost 2500,00, unrealized +250,00 -> +10,0 %
    wrap(
      <PositionsTable
        positions={[
          makePosition({ cost_minor: 250_000, unrealized_pnl_minor: 25_000 }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const amount = screen.getByTestId("position-profit-amount");
    expect(norm(amount.textContent ?? "")).toBe(
      norm(formatMinor(25_000, "USD")),
    );
    expect(amount.className).toContain("text-emerald-500");
    expect(
      norm(screen.getByTestId("position-profit-percent").textContent ?? ""),
    ).toBe(norm("+10,0 %"));
  });

  it("shows unrealized loss in red with a negative percentage", () => {
    // cost 2500,00, unrealized -300,00 -> -12,0 %
    wrap(
      <PositionsTable
        positions={[
          makePosition({ cost_minor: 250_000, unrealized_pnl_minor: -30_000 }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const amount = screen.getByTestId("position-profit-amount");
    expect(norm(amount.textContent ?? "")).toBe(
      norm(formatMinor(-30_000, "USD")),
    );
    expect(amount.className).toContain("text-red-500");
    expect(
      norm(screen.getByTestId("position-profit-percent").textContent ?? ""),
    ).toBe(norm("-12,0 %"));
  });

  it("shows a dash with a tooltip for the profit column when unrealized_pnl_minor is null", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            market_value_minor: null,
            market_value_currency: null,
            unrealized_pnl_minor: null,
            market_value_gap: "no_quote",
          }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const dash = screen.getByTestId("position-profit-dash");
    expect(dash).toHaveTextContent("—");
    // The profit is missing because the valuation is; it says so and defers
    // to the valuation's reason (see below).
    expect(dash).toHaveAttribute(
      "title",
      `${PROFIT_NEEDS_VALUATION}\n${NO_VALUATION.noQuote}`,
    );
    expect(
      screen.queryByTestId("position-profit-amount"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("position-profit-percent"),
    ).not.toBeInTheDocument();
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
    expect(norm(marketValue.textContent ?? "")).toBe(
      norm(formatMinor(952_00, "USD")),
    );

    const dash = screen.getByTestId("position-profit-dash");
    expect(dash).toHaveTextContent("—");
    expect(dash).toHaveAttribute(
      "title",
      "Оценка в другой валюте — прибыль не рассчитывается",
    );
    expect(
      screen.queryByTestId("position-profit-amount"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("position-profit-percent"),
    ).not.toBeInTheDocument();
  });

  describe("the empty valuation cell says WHICH absence it is", () => {
    // #78: three things leave a position unvalued, and «Нет котировки» was true of
    // only one. The server names it (market_value_gap); each test pins the exact
    // sentence and that it is not the others.
    const withNoValuation = (gap: Position["market_value_gap"]) =>
      makePosition({
        market_value_minor: null,
        market_value_currency: null,
        price: null,
        price_on: null,
        unrealized_pnl_minor: null,
        market_value_gap: gap,
      });

    it("says a quote is missing only where a quote is what is missing", () => {
      wrap(
        <PositionsTable
          positions={[withNoValuation("no_quote")]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      expect(screen.getByTestId("position-no-quote")).toHaveAttribute(
        "title",
        NO_VALUATION.noQuote,
      );
    });

    it("does not blame a missing quote for a type it does not price", () => {
      wrap(
        <PositionsTable
          positions={[withNoValuation("type_not_priced")]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      const dash = screen.getByTestId("position-no-quote");
      expect(dash).toHaveAttribute("title", NO_VALUATION.typeNotPriced);
      // It must not send the reader after a quote, nor promise a figure.
      expect(dash.getAttribute("title")).not.toBe(NO_VALUATION.noQuote);
      expect(dash.getAttribute("title")).not.toMatch(
        /появится|посчитается сама|пока нет/,
      );
    });

    it("does not blame a missing quote for a bond with no face value", () => {
      wrap(
        <PositionsTable
          positions={[withNoValuation("no_face_value")]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      const dash = screen.getByTestId("position-no-quote");
      expect(dash).toHaveAttribute("title", NO_VALUATION.noFaceValue);
      expect(dash.getAttribute("title")).not.toBe(NO_VALUATION.noQuote);
      expect(dash.getAttribute("title")).not.toBe(NO_VALUATION.typeNotPriced);
    });

    it("claims no cause for one it cannot name, and none for one the server did not send", () => {
      // #105: an unknown value (newer server) or a null cause (older one) must
      // not read «Нет котировки».
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              instrument: {
                ...makePosition().instrument,
                id: "instr-unnameable",
              },
              market_value_minor: null,
              market_value_currency: null,
              unrealized_pnl_minor: null,
              market_value_gap:
                "no_lunar_settlement_price" as Position["market_value_gap"],
            }),
            makePosition({
              instrument: {
                ...makePosition().instrument,
                id: "instr-no-cause",
              },
              market_value_minor: null,
              market_value_currency: null,
              unrealized_pnl_minor: null,
              market_value_gap: null,
            }),
          ]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      const [unnameable, noCause] = screen.getAllByTestId("position-no-quote");
      expect(unnameable).toHaveAttribute("title", NO_VALUATION.general);
      expect(noCause).toHaveAttribute("title", NO_VALUATION.general);
    });

    it("gives the profit dash the valuation's cause, not «нет котировки»", () => {
      // The profit dash takes the valuation's reason (#78).
      wrap(
        <PositionsTable
          positions={[withNoValuation("type_not_priced")]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      const dash = screen.getByTestId("position-profit-dash");
      expect(dash).toHaveAttribute(
        "title",
        `${PROFIT_NEEDS_VALUATION}\n${NO_VALUATION.typeNotPriced}`,
      );
      expect(dash.getAttribute("title")).not.toContain(NO_VALUATION.noQuote);
    });

    it("keeps the currency-mismatch sentence on the profit dash of a row that HAS a valuation", () => {
      // With a valuation present, the profit's own reason stands.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "RUB",
              market_value_minor: 952_00,
              market_value_currency: "USD",
              unrealized_pnl_minor: null,
              market_value_gap: "no_rate_valuation_currency",
            }),
          ]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      const dash = screen.getByTestId("position-profit-dash");
      expect(dash).toHaveAttribute(
        "title",
        "Оценка в другой валюте — прибыль не рассчитывается",
      );
      expect(dash.getAttribute("title")).not.toContain(PROFIT_NEEDS_VALUATION);
    });
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
    expect(norm(amount.textContent ?? "")).toBe(
      norm(formatMinor(305_50, "USD")),
    );
    // ...profit is computed normally (not the currency-mismatch dash)...
    const profitAmount = screen.getByTestId("position-profit-amount");
    expect(norm(profitAmount.textContent ?? "")).toBe(
      norm(formatMinor(25_000, "USD")),
    );
    // ...and the source amount appears only in the tooltip, never as text.
    const sourceAmount = formatMinor(2_800_00, "RUB");
    expect(screen.queryByText(sourceAmount)).not.toBeInTheDocument();
    const priceLine = screen.getByTestId("position-price");
    expect(norm(priceLine.getAttribute("title") ?? "")).toBe(
      norm(
        `Цена на 20.07.2026\n${PRICE_SESSION_NOTE}\n${PRICE_VALUATION_NOTE}\nПересчитано из ${sourceAmount}`,
      ),
    );
  });

  it("omits the converted-from tooltip line when the market value has no source currency/amount", () => {
    wrap(
      <PositionsTable
        positions={[makePosition()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const priceLine = screen.getByTestId("position-price");
    expect(priceLine.getAttribute("title")).toBe(
      `Цена на 20.07.2026\n${PRICE_SESSION_NOTE}\n${PRICE_VALUATION_NOTE}`,
    );
  });

  // A bond's Position.price is a percentage of face (marketValue: face × price/100
  // × quantity). The demo's ОФЗ 26238: face 1 000,00 ₽, quote 95.20, 952 ₽ a bond.
  // The unit is stated (#32); no money is derived on the client. The caption states
  // the rule, since the cell above is that product only in native mode.
  const BOND_PRICE_NOTE =
    "Облигация котируется в процентах от номинала, а не в деньгах за штуку: одна бумага стоит номинал, умноженный на этот процент";
  const BOND_MONEY_NOTE =
    "Деньги за одну бумагу — это номинал, умноженный на этот процент. Номинал записан в каталоге и не обновляется: у амортизируемой облигации он со временем уменьшается, и тогда цена в деньгах будет завышена";

  function makeBond(overrides: Partial<Position> = {}): Position {
    return makePosition({
      instrument: {
        id: "instr-bond",
        type: "bond",
        name: "ОФЗ 26238",
        ticker: "SU26238RMFS4",
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
      settled_minor: null,
      total_minor: null,
      market_value_currency: "RUB",
      unrealized_pnl_minor: 520_000,
      price: "95.20",
      price_on: "2026-07-20",
      // 1 000,00 ₽ at 95.20 % = 952,00 ₽, struck by the server.
      price_money_minor: 95_200,
      ...overrides,
    });
  }

  // «Прибыль» is the unrealized half, «Зафиксировано» the realized result plus
  // income, «Всего» both. Three different numbers, so a miswired cell shows.
  it("keeps the profit, the settled result and their total apart", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            currency: "USD",
            cost_minor: 250_000,
            unrealized_pnl_minor: 25_000,
            realized_pnl_minor: 10_000,
            income_minor: 1_000,
            income_by_currency: [{ currency: "USD", income_minor: 1_000 }],
          }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    // Unrealized alone.
    expect(
      norm(screen.getByTestId("position-profit-amount").textContent ?? ""),
    ).toBe(norm(formatMinor(25_000, "USD")));
    // Realized 100,00 $ + income 10,00 $ = 110,00 $, and NOT the 250,00 $ of
    // profit that shares the row.
    expect(norm(screen.getByTestId("position-settled").textContent ?? "")).toBe(
      norm(formatMinor(11_000, "USD")),
    );
    // Everything: 110,00 $ settled + 250,00 $ still on paper.
    expect(norm(screen.getByTestId("position-total").textContent ?? "")).toBe(
      norm(formatMinor(36_000, "USD")),
    );
  });

  it("says under the profit what this paper has already locked in", () => {
    // A closed position's profit is nothing; the sale's result gets its own
    // line under it.
    wrap(
      <PositionsTable
        positions={[
          makeClosed({ realized_pnl_minor: 1_326_400, currency: "RUB" }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );
    // Closed rows are behind the control, so the row has to be asked for first.
    fireEvent.click(screen.getByTestId("toggle-closed-positions"));

    const realized = screen.getByTestId("position-realized");
    expect(norm(realized.textContent ?? "")).toContain(
      norm(formatMinor(1_326_400, "RUB")),
    );
  });

  it("draws no realized line on a paper that has never been sold out of", () => {
    // «реализовано 0,00» on every open row is noise, and noise is what makes a
    // real line invisible.
    wrap(
      <PositionsTable
        positions={[makePosition({ realized_pnl_minor: 0 })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByTestId("position-profit-amount")).toBeInTheDocument();
    expect(screen.queryByTestId("position-realized")).not.toBeInTheDocument();
  });

  it("spells out both terms of the settled figure, and what the tax does and does not include", () => {
    // The tooltip says what the cell is made of, as one exact string: the
    // dividend's tax is inside the income (Position.income_minor), the account's
    // withheld tax is not (RealizedTotal.tax_withheld_by_currency).
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            currency: "USD",
            realized_pnl_minor: 10_000,
            income_minor: 1_000,
            income_by_currency: [{ currency: "USD", income_minor: 1_000 }],
          }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const cell = screen.getByTestId("position-settled").closest("td");
    expect(norm(cell?.getAttribute("title") ?? "")).toBe(
      norm(
        "Итог закрытого: реализованная прибыль 100,00 $ плюс доход 10,00 $. От цен рынка он не зависит, а меняется, только если меняется сама история — например, добавили операцию задним числом или стала известна дата расчётов по сделке. Налог с дивиденда или купона уже вычтен из дохода. А тот, что брокер списывает со счёта при выводе средств, тут не вычтен — он берётся с накопленной за год базы, а не с бумаги, и показан отдельной суммой в шапке счёта",
      ),
    );
  });

  // The dash under «Зафиксировано» has two causes: a disposal settled in another
  // currency, or income in several currencies.
  it("blames the sale when it is the sale that has no common currency", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            currency: "USD",
            realized_pnl_minor: null,
            settled_minor: null,
          }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const dash = screen.getByTestId("position-settled-dash");
    expect(announcedText(dash)).toContain(
      "расчёт по одной из продаж пришёл в другой валюте",
    );
    expect(announcedText(dash)).not.toContain("выплаты");
  });

  it("blames the payments when the sale is not what stopped it", () => {
    // A yuan bond paid coupons in roubles: nothing was sold.
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            currency: "CNY",
            realized_pnl_minor: 0,
            settled_minor: null,
            income_minor: 0,
            income_by_currency: [{ currency: "RUB", income_minor: 141_075 }],
          }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const dash = screen.getByTestId("position-settled-dash");
    expect(announcedText(dash)).toContain(
      "выплаты по этой бумаге пришли не только в её валюте",
    );
    expect(announcedText(dash)).not.toContain("продаж");
    // And the payments it blames are on the screen beside it, which is what
    // the sentence promises.
    expect(
      norm(
        screen.getByTestId("position-income-other-currency").textContent ?? "",
      ),
    ).toContain(norm(formatMinor(141_075, "RUB")));
  });

  // A price months old still feeds every total (frozen funds valued from
  // 25.02.2022); its age is shown on the row. Dates are relative to the clock.
  it("says out loud how old a price is once it is months out of date", () => {
    wrap(
      <PositionsTable
        positions={[makePosition({ price_on: daysAgo(400) })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const stale = screen.getByTestId("position-price-stale");
    expect(stale.textContent).toContain("цена не обновлялась");
    // The price itself is still shown: this is a caveat, not a replacement.
    expect(screen.getByTestId("position-price")).toBeInTheDocument();
  });

  // Shares with no purchase price count as bought for nothing; the paper says
  // so under its cost, with a hint where the real price is.
  it("says on the paper that its purchase price is unknown and counted as nought", () => {
    wrap(
      <PositionsTable
        positions={[makePosition({ cost_minor: 0, has_unknown_cost: true })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const note = screen.getByTestId("position-unknown-cost");
    expect(note.textContent).toContain("цена покупки неизвестна");
    expect(note.textContent).toContain("0");
    expect(note.getAttribute("title")).toContain("отчёте брокера");
  });

  it("offers to give the price where the reader can write, and hands over the paper", () => {
    const opened: unknown[] = [];
    wrap(
      <PositionsTable
        positions={[makePosition({ cost_minor: 0, has_unknown_cost: true })]}
        mode="native"
        baseCurrency="RUB"
        onPriceUnknown={(paper) => opened.push(paper)}
      />,
    );
    fireEvent.click(screen.getByTestId("position-unknown-cost-action"));
    expect(opened).toHaveLength(1);
    expect(opened[0]).toMatchObject({ name: makePosition().instrument.name });
  });

  it("offers nothing to a reader who cannot write", () => {
    wrap(
      <PositionsTable
        positions={[makePosition({ cost_minor: 0, has_unknown_cost: true })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );
    expect(screen.getByTestId("position-unknown-cost")).toBeInTheDocument();
    expect(
      screen.queryByTestId("position-unknown-cost-action"),
    ).not.toBeInTheDocument();
  });

  it("says nothing about the purchase price when it is known", () => {
    wrap(
      <PositionsTable
        positions={[makePosition()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );
    expect(
      screen.queryByTestId("position-unknown-cost"),
    ).not.toBeInTheDocument();
  });

  it("says nothing about the age of an ordinary price", () => {
    // A few days or weeks is no news; warning on every row hides real ones.
    wrap(
      <PositionsTable
        positions={[makePosition({ price_on: daysAgo(3) })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByTestId("position-price")).toBeInTheDocument();
    expect(
      screen.queryByTestId("position-price-stale"),
    ).not.toBeInTheDocument();
  });

  // Cash is a holding: bought at one rate, worth another today.
  function makeCash(overrides: Partial<CashPosition> = {}): CashPosition {
    return {
      currency: "USD",
      amount_minor: 150_000,
      in_base: {
        currency: "RUB",
        value_minor: 13_500_000,
        cost_minor: 9_000_000,
        unrealized_pnl_minor: 4_500_000,
        realized_pnl_minor: 0,
        gap: null,
      },
      ...overrides,
    };
  }

  it("shows the money among the papers, with its own name and kind", () => {
    wrap(
      <PositionsTable
        positions={[makePosition()]}
        cash={[makeCash()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByTestId("cash-currency").textContent).toBe(
      "Деньги · USD",
    );
    expect(norm(screen.getByTestId("cash-amount").textContent ?? "")).toBe(
      norm(formatMinor(150_000, "USD")),
    );
    // The paper is still there: money joins the list, it does not replace it.
    expect(screen.getByText("Test Corp")).toBeInTheDocument();
  });

  it("leaves the money columns empty in the account's own currencies", () => {
    // A thousand dollars is a thousand dollars: one figure, not three.
    wrap(
      <PositionsTable
        positions={[]}
        cash={[makeCash()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByTestId("cash-cost").textContent).toBe("");
    expect(screen.getByTestId("cash-value").textContent).toBe("");
    expect(screen.queryByTestId("cash-profit")).not.toBeInTheDocument();
  });

  it("shows what the money cost, what it is worth and the difference, in the base currency", () => {
    wrap(
      <PositionsTable
        positions={[]}
        cash={[makeCash()]}
        mode="base"
        baseCurrency="RUB"
      />,
    );

    expect(norm(screen.getByTestId("cash-cost").textContent ?? "")).toBe(
      norm(formatMinor(9_000_000, "RUB")),
    );
    expect(norm(screen.getByTestId("cash-value").textContent ?? "")).toBe(
      norm(formatMinor(13_500_000, "RUB")),
    );
    // The whole point of the row: the currency's own move while the money sat
    // there. 45 000 ₽ made on dollars nobody traded.
    expect(norm(screen.getByTestId("cash-profit").textContent ?? "")).toBe(
      norm(formatMinor(4_500_000, "RUB")),
    );
    // Still the balance in its own currency, never converted: the quantity
    // column of a holding is the holding, not its price.
    expect(norm(screen.getByTestId("cash-amount").textContent ?? "")).toBe(
      norm(formatMinor(150_000, "USD")),
    );
  });

  it("says «нет курса» where the money's profit would have been", () => {
    // The owner's gold under XAU, which the Bank of Russia does not quote: the
    // row names the currency, as the account total does.
    wrap(
      <PositionsTable
        positions={[]}
        cash={[
          makeCash({
            currency: "XAU",
            amount_minor: 2_600,
            in_base: {
              currency: "RUB",
              value_minor: null,
              cost_minor: null,
              unrealized_pnl_minor: null,
              realized_pnl_minor: null,
              gap: "no_rate_today",
            },
          }),
        ]}
        mode="base"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByTestId("cash-no-rate")).toBeInTheDocument();
    // And not the overdraft's sentence, which is about a different absence.
    expect(screen.queryByTestId("cash-overdraft")).not.toBeInTheDocument();
  });

  it("says «в минусе» rather than «нет курса» on an overdraft", () => {
    // An overdraft and an unpriceable currency both leave the profit empty for
    // different reasons; the rate sentence stays off the overdraft.
    wrap(
      <PositionsTable
        positions={[]}
        cash={[
          makeCash({
            amount_minor: -40_000,
            in_base: {
              currency: "RUB",
              value_minor: -3_600_000,
              cost_minor: 0,
              unrealized_pnl_minor: null,
              realized_pnl_minor: 0,
              gap: "negative_balance",
            },
          }),
        ]}
        mode="base"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByTestId("cash-overdraft")).toBeInTheDocument();
    expect(screen.queryByTestId("cash-no-rate")).not.toBeInTheDocument();
  });

  it("says nothing about a profit on the base currency itself", () => {
    // Roubles in a rouble space: the balance only, no row of noughts.
    wrap(
      <PositionsTable
        positions={[]}
        cash={[
          makeCash({
            currency: "RUB",
            amount_minor: 500_000,
            in_base: {
              currency: "RUB",
              value_minor: 500_000,
              cost_minor: 500_000,
              unrealized_pnl_minor: 0,
              realized_pnl_minor: 0,
              gap: null,
            },
          }),
        ]}
        mode="base"
        baseCurrency="RUB"
      />,
    );

    expect(norm(screen.getByTestId("cash-amount").textContent ?? "")).toBe(
      norm(formatMinor(500_000, "RUB")),
    );
    expect(screen.queryByTestId("cash-profit")).not.toBeInTheDocument();
  });

  it("says under the profit what this money has already earned", () => {
    // Dollars bought and sold back: the current figure is a true nought and
    // the result is in what already left.
    wrap(
      <PositionsTable
        positions={[]}
        cash={[
          makeCash({
            amount_minor: 0,
            in_base: {
              currency: "RUB",
              value_minor: 0,
              cost_minor: 0,
              unrealized_pnl_minor: 0,
              realized_pnl_minor: 3_000_000,
              gap: null,
            },
          }),
        ]}
        mode="base"
        baseCurrency="RUB"
      />,
    );
    fireEvent.click(screen.getByTestId("toggle-closed-positions"));

    expect(
      norm(screen.getByTestId("cash-realized").textContent ?? ""),
    ).toContain(norm(formatMinor(3_000_000, "RUB")));
  });

  it("draws no realized line on money that has never moved", () => {
    wrap(
      <PositionsTable
        positions={[]}
        cash={[makeCash()]}
        mode="base"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByTestId("cash-profit")).toBeInTheDocument();
    expect(screen.queryByTestId("cash-realized")).not.toBeInTheDocument();
  });

  it("shows a negative balance rather than hiding it, and says what it is", () => {
    // The owner's case: yuan spent without a known purchase, a balance below
    // nought, shown as such.
    wrap(
      <PositionsTable
        positions={[]}
        cash={[makeCash({ currency: "CNY", amount_minor: -40_000 })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(norm(screen.getByTestId("cash-amount").textContent ?? "")).toBe(
      norm(formatMinor(-40_000, "CNY")),
    );
    // And says why there is no profit: the journal is missing operations.
    expect(screen.getByTestId("cash-overdraft")).toBeInTheDocument();
  });

  it("draws no overdraft note on money the account actually holds", () => {
    wrap(
      <PositionsTable
        positions={[]}
        cash={[makeCash()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByTestId("cash-amount")).toBeInTheDocument();
    expect(screen.queryByTestId("cash-overdraft")).not.toBeInTheDocument();
  });

  it("hides a currency the account holds nothing of, behind the same control", () => {
    // An emptied currency is history, behind the closed-rows control.
    wrap(
      <PositionsTable
        positions={[makePosition(), makeClosed()]}
        cash={[makeCash({ amount_minor: 0 })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.queryByTestId("cash-row")).not.toBeInTheDocument();
    // The count names both closed papers and emptied currencies.
    expect(screen.getByTestId("closed-positions-note").textContent).toContain(
      "2",
    );

    fireEvent.click(screen.getByTestId("toggle-closed-positions"));
    expect(screen.getByTestId("cash-row")).toBeInTheDocument();
  });

  // A closed position is history hidden by default; the count beside the
  // control makes hiding honest.
  function makeClosed(overrides: Partial<Position> = {}): Position {
    return makePosition({
      instrument: {
        ...makePosition().instrument,
        id: "instr-closed",
        name: "Проданное",
      },
      quantity: "0",
      cost_minor: 0,
      market_value_minor: null,
      market_value_currency: null,
      market_value_gap: "no_quote",
      unrealized_pnl_minor: null,
      realized_pnl_minor: 1_326_400,
      ...overrides,
    });
  }

  it("keeps a sold-out position off the list until asked, and says how many are missing", () => {
    wrap(
      <PositionsTable
        positions={[makePosition(), makeClosed()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByText("Test Corp")).toBeInTheDocument();
    expect(screen.queryByText("Проданное")).not.toBeInTheDocument();
    // The hidden count, and that the totals are not filtered.
    expect(screen.getByTestId("closed-positions-note").textContent).toBe(
      "скрыто: 1 — закрытые бумаги и валюты с нулевым остатком. Итоги считаются по всем позициям, включая скрытые",
    );
  });

  it("shows the closed rows once the control is pressed, and changes its word", () => {
    wrap(
      <PositionsTable
        positions={[makePosition(), makeClosed()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const toggle = screen.getByTestId("toggle-closed-positions");
    expect(toggle.textContent).toBe("Показать закрытые");
    fireEvent.click(toggle);

    expect(screen.getByText("Проданное")).toBeInTheDocument();
    expect(screen.getByText("Test Corp")).toBeInTheDocument();
    expect(toggle.textContent).toBe("Скрыть закрытые");
    expect(screen.getByTestId("closed-positions-note").textContent).toBe(
      "закрытых и пустых среди них: 1",
    );

    // And back: the control is a switch, not a one-way door.
    fireEvent.click(toggle);
    expect(screen.queryByText("Проданное")).not.toBeInTheDocument();
  });

  it("offers no control at all when nothing is closed", () => {
    // A button that hides nothing, over a sentence that would have to say
    // «скрыто закрытых: 0», is a question nobody asked.
    wrap(
      <PositionsTable
        positions={[makePosition()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.getByText("Test Corp")).toBeInTheDocument();
    expect(
      screen.queryByTestId("toggle-closed-positions"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("closed-positions-note"),
    ).not.toBeInTheDocument();
  });

  it("marks a bond's price as a percentage of face value and says so in the tooltip", () => {
    wrap(
      <PositionsTable
        positions={[makeBond()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const priceLine = screen.getByTestId("position-price");
    // Money first, then the percent: 952,00 ₽ is the server's
    // price_money_minor in the face currency; the percent has no currency.
    expect(norm(priceLine.textContent ?? "")).toBe("952,00 ₽ · 95,20 %");
    // The raw NBSP characters too, as in ru.json's priceMoneyAndPercent;
    // the spaces around "·" may break.
    expect(priceLine.textContent).toBe("952,00\u00a0₽ · 95,20\u00a0%");
    // Session note, then the percentage note, then the valuation note; the
    // face-value caveat only with the money.
    expect(norm(priceLine.getAttribute("title") ?? "")).toBe(
      norm(
        `Цена на 20.07.2026\n${PRICE_SESSION_NOTE}\n${BOND_PRICE_NOTE}\n${BOND_MONEY_NOTE}\n${PRICE_VALUATION_NOTE}`,
      ),
    );
  });

  it("prints the percentage alone, and no face-value caveat, when the server sent no money price", () => {
    // No price_money_minor: the percent alone, no client multiplication, and
    // no currency sign on it (#76). The face caveat goes with the money only.
    wrap(
      <PositionsTable
        positions={[makeBond({ price_money_minor: null })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const priceLine = screen.getByTestId("position-price");
    expect(priceLine.textContent).toBe("95,20\u00a0%");
    expect(priceLine.textContent).not.toContain("₽");
    expect(norm(priceLine.getAttribute("title") ?? "")).toBe(
      norm(
        `Цена на 20.07.2026\n${PRICE_SESSION_NOTE}\n${BOND_PRICE_NOTE}\n${PRICE_VALUATION_NOTE}`,
      ),
    );
    expect(priceLine.getAttribute("title")).not.toContain(BOND_MONEY_NOTE);
  });

  it.each([["share"], ["etf"]] as const)(
    "writes a %s's price in the currency it is quoted in, never as a percentage",
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
      expect(norm(priceLine.textContent ?? "")).toBe("305,50 $");
      expect(priceLine.textContent).not.toContain("%");
      expect(priceLine.getAttribute("title")).toBe(
        `Цена на 20.07.2026\n${PRICE_SESSION_NOTE}\n${PRICE_VALUATION_NOTE}`,
      );
      expect(priceLine.getAttribute("title")).not.toContain(BOND_PRICE_NOTE);
    },
  );

  // #76: in base mode the valuation converts and the price does not, so a
  // foreign share's price names its currency. 10 × $305,50 = $3 055,00, the
  // 274 950,00 ₽ above at 90 ₽/$; far apart on purpose.
  it("names the currency of a share's price under a base-currency valuation", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            currency: "USD",
            in_base: {
              cost_minor: 22_500_000,
              market_value_minor: 27_495_000,
              settled_minor: null,
              total_minor: null,
              unrealized_pnl_minor: 4_995_000,
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

    expect(
      norm(screen.getByTestId("position-market-value").textContent ?? ""),
    ).toBe(norm(formatMinor(27_495_000, "RUB")));
    const priceLine = screen.getByTestId("position-price");
    expect(norm(priceLine.textContent ?? "")).toBe("305,50 $");
    // The one wrong sign that would look plausible: the valuation's own, taken
    // from the cell above instead of from the field that describes the price.
    expect(priceLine.textContent).not.toContain("₽");
  });

  // Where the quote's currency differs from the position's, the price takes
  // market_value_source_currency, not market_value_currency (the converted
  // figure's).
  it("takes a share price's currency from the quote, not from the converted valuation", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
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
    expect(norm(priceLine.textContent ?? "")).toBe("305,50 ₽");
    expect(priceLine.textContent).not.toContain("$");
  });

  // A sub-cent quote keeps its digits with a currency sign ($0.0025 is not
  // «0,00 $», #30).
  it("keeps a sub-cent quote's digits when it gains a currency sign", () => {
    wrap(
      <PositionsTable
        positions={[makePosition({ price: "0.0025", market_value_minor: 2 })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const priceLine = screen.getByTestId("position-price");
    expect(norm(priceLine.textContent ?? "")).toBe("0,0025 $");
    expect(norm(priceLine.textContent ?? "")).not.toBe("0,00 $");
  });

  // A type this build cannot read a price for gets no currency claimed (#105);
  // today's server values only share, etf and bond.
  it("claims no currency for a priced type this build does not know", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({
            instrument: { ...makePosition().instrument, type: "crypto" },
          }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const priceLine = screen.getByTestId("position-price");
    expect(norm(priceLine.textContent ?? "")).toBe("305,50");
    expect(priceLine.textContent).not.toContain("$");
    expect(priceLine.textContent).not.toContain("%");
  });

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
    // A bond: the percent belongs to no currency, the money to the face
    // currency (#76).
    expect(norm(priceLine.textContent ?? "")).toBe("952,00 ₽ · 95,20 %");
    expect(priceLine.textContent).not.toContain("$");
    expect(norm(priceLine.getAttribute("title") ?? "")).toBe(
      norm(
        `Цена на 20.07.2026\n${PRICE_SESSION_NOTE}\n${BOND_PRICE_NOTE}\n${BOND_MONEY_NOTE}\n${PRICE_VALUATION_NOTE}\nПересчитано из ${sourceAmount}`,
      ),
    );
  });

  it("omits the percentage (but still shows the amount) when cost is 0", () => {
    wrap(
      <PositionsTable
        positions={[
          makePosition({ cost_minor: 0, unrealized_pnl_minor: 1_000 }),
        ]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    const amount = screen.getByTestId("position-profit-amount");
    expect(norm(amount.textContent ?? "")).toBe(
      norm(formatMinor(1_000, "USD")),
    );
    expect(
      screen.queryByTestId("position-profit-percent"),
    ).not.toBeInTheDocument();
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
                settled_minor: 9_100,
                total_minor: 236_600,
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
      expect(
        norm(screen.getByTestId("position-market-value").textContent ?? ""),
      ).toBe(norm(formatMinor(2_780_050, "RUB")));
      expect(
        norm(screen.getByTestId("position-profit-amount").textContent ?? ""),
      ).toBe(norm(formatMinor(227_500, "RUB")));
      expect(
        norm(screen.getByTestId("position-settled").textContent ?? ""),
      ).toBe(norm(formatMinor(9_100, "RUB")));
      // No not-converted indicators; checked by test id, since the marker's text
      // lives in a title.
      for (const testId of [
        "position-cost",
        "position-market-value",
        "position-profit-amount",
        "position-settled",
      ]) {
        expect(
          screen.queryByTestId(`${testId}-not-converted`),
        ).not.toBeInTheDocument();
      }
    });

    it("computes the profit percentage from the converted cost/profit pair, not the native one", () => {
      // Native: 2500,00 cost, 250,00 profit -> +10,0 %. Both scale by 8 here, so
      // only mixing a native figure with a converted one gives a wrong percentage. Real
      // data usually differs (see the next test).
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
                settled_minor: null,
                total_minor: null,
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

      expect(
        norm(screen.getByTestId("position-profit-percent").textContent ?? ""),
      ).toBe(norm("+10,0 %"));
    });

    it("omits the percentage when profit stays native while cost converts", () => {
      // in_base has a null unrealized_pnl_minor here, so cost is RUB and profit
      // USD; no percentage rather than a wrong one.
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
                settled_minor: null,
                total_minor: null,
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

      // The amount is still shown in its own currency; the sentence is a
      // tooltip plus a screen-reader copy (#31).
      expect(
        norm(visibleText(screen.getByTestId("position-profit-amount"))),
      ).toBe(norm(formatMinor(25_000, "USD")));
      expect(
        screen.getByTestId("position-profit-amount-not-converted"),
      ).toBeInTheDocument();
      expect(
        screen.queryByTestId("position-profit-percent"),
      ).not.toBeInTheDocument();
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
      expect(
        norm(screen.getByTestId("position-cost").textContent ?? ""),
      ).toContain(norm(formatMinor(250_000, "USD")));
      expect(
        norm(screen.getByTestId("position-market-value").textContent ?? ""),
      ).toContain(norm(formatMinor(305_50, "USD")));
      expect(
        norm(screen.getByTestId("position-profit-amount").textContent ?? ""),
      ).toContain(norm(formatMinor(25_000, "USD")));
      expect(
        norm(screen.getByTestId("position-settled").textContent ?? ""),
      ).toContain(norm(formatMinor(1_000, "USD")));

      expect(screen.getByTestId("position-cost-not-converted")).toHaveAttribute(
        "title",
        CAPTION.noRateLotDate,
      );
      expect(
        screen.getByTestId("position-market-value-not-converted"),
      ).toBeInTheDocument();
      expect(
        screen.getByTestId("position-profit-amount-not-converted"),
      ).toBeInTheDocument();
      expect(
        screen.getByTestId("position-settled-not-converted"),
      ).toBeInTheDocument();
    });

    // #66: each case pins its own sentence on every cell and that it is none
    // of the others.
    describe("the caption names the term the server stopped on", () => {
      // captionsFor renders one position failing for `gap` and returns the four
      // markers' titles, unmounting any previous render first.
      function captionsFor(
        gap: NonNullable<Position["in_base_gap"]>,
      ): string[] {
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
        return ROW_MARKERS.map(
          (id) => screen.getByTestId(id).getAttribute("title") ?? "",
        );
      }

      it("says an unrecorded purchase date, and does not promise it will resolve on its own", () => {
        for (const title of captionsFor("undated_lot")) {
          expect(title).toBe(CAPTION.undatedLot);
        }
        // Nothing closes this gap by itself, but it can be fixed (sell the lot,
        // re-enter the transfer), so not "never", only "not on its own".
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

      it("makes the figure's return conditional on the rate for every closeable cause, and offers it for no other", () => {
        // The closeable causes say the figure follows if a rate appears, as a
        // condition (#105), since rates come from one source that may never publish a
        // pair; the permanent one says only that nothing fixes it automatically.
        // Asserted as a property of the four sentences.
        for (const gap of [
          "no_rate_lot_date",
          "no_rate_income_date",
          "no_rate_today",
        ] as const) {
          const [cost] = captionsFor(gap);
          expect(cost).toContain("Если курс появится при обновлении курсов");
          // The bare promise, in the exact shape it had.
          expect(cost).not.toContain("Курс появится при обновлении курсов");
          expect(cost).not.toContain("никогда");
        }
        const [undated] = captionsFor("undated_lot");
        expect(undated).toContain("уже неоткуда");
      });

      it("does not claim a rate lookup that may never have happened", () => {
        // These two say only that the earlier term did not stop the sum, never
        // that purchase-day rates were found.
        for (const gap of ["no_rate_income_date", "no_rate_today"] as const) {
          const [cost] = captionsFor(gap);
          expect(cost).toContain("Дело не в стоимости");
          expect(cost).not.toContain("нашлись");
        }
      });
    });

    it("captions the row from in_base_gap alone, never from has_undated_lots", () => {
      // Deliberately impossible rows, to prove which field the caption reads.
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
              instrument: {
                ...makePosition().instrument,
                id: "instr-flag-off",
              },
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

      const [flagOn, flagOff] = screen.getAllByTestId(
        "position-cost-not-converted",
      );
      expect(flagOn).toHaveAttribute("title", CAPTION.noRateLotDate);
      expect(flagOff).toHaveAttribute("title", CAPTION.undatedLot);
    });

    it("falls back to a phrase that names no cause at all for one this build cannot name", () => {
      // An unknown value degrades to a vague true sentence, not a blank, a crash
      // or a named cause; the second row is a missing cause (older server).
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              instrument: {
                ...makePosition().instrument,
                id: "instr-unknown-gap",
              },
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

      for (const marker of screen.getAllByTestId(
        "position-cost-not-converted",
      )) {
        expect(marker).toHaveAttribute("title", CAPTION.general);
      }
      // #105 as a property: the fallback names no cause, «курс» in any form
      // included.
      expect(CAPTION.general).not.toContain("Нет курса");
      expect(CAPTION.general.toLowerCase()).not.toContain("курс");
      expect(CAPTION.general).toContain("не посчиталась");
      // Still marked, still showing the native figure: degrading the wording
      // must not degrade the disclosure.
      expect(
        screen.getAllByTestId("position-settled-not-converted"),
      ).toHaveLength(2);
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
      expect(
        screen.queryByTestId("position-cost-not-converted"),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByTestId("position-market-value-not-converted"),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByTestId("position-profit-amount-not-converted"),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByTestId("position-settled-not-converted"),
      ).not.toBeInTheDocument();
    });

    it("gives the valuation its own cause even when the position's currency already is the base currency", () => {
      // market_value_gap on a position already in the base currency
      // (TestPositionMarketValueGapPublishedOnABaseCurrencyPosition): the
      // valuation's cause must outrank the row's general fallback.
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
      expect(
        screen.queryByTestId("position-cost-not-converted"),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByTestId("position-profit-amount-not-converted"),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByTestId("position-settled-not-converted"),
      ).not.toBeInTheDocument();

      // The valuation alone carries a marker, and it names its OWN cause —
      // not the general phrase the row would otherwise fall back to.
      const valuationMarker = screen.getByTestId(
        "position-market-value-not-converted",
      );
      expect(valuationMarker).toHaveAttribute(
        "title",
        CAPTION.valuationCurrency,
      );
      expect(valuationMarker.getAttribute("title")).not.toBe(CAPTION.general);
    });

    it("claims today's rate only for the market value, and names the historical rates behind cost, income and profit", () => {
      // in_base.rate_on dates the valuation only; the other three cells use
      // their own historical wording.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              in_base: {
                cost_minor: 2_275_000,
                market_value_minor: 2_780_050,
                settled_minor: 9_100,
                total_minor: 236_600,
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
      // Settled is realized plus income, all past events, so the sentence names
      // all three kinds of date.
      expect(screen.getByTestId("position-settled")).toHaveAttribute(
        "title",
        "Пересчитано по курсам на даты событий — покупок, продаж и выплат, — а не по текущему",
      );
      // The total adds the unrealized half, which is today's valuation at
      // today's rate: one sentence over both halves would be false about one.
      expect(screen.getByTestId("position-total")).toHaveAttribute(
        "title",
        "Половины пересчитаны по разным курсам: свершившееся — по курсам на даты событий, рыночная оценка внутри — по сегодняшнему",
      );
      expect(screen.getByTestId("position-profit-amount")).toHaveAttribute(
        "title",
        "Оценка по текущему курсу минус стоимость по курсам на даты покупок — поэтому включает изменение курса",
      );
      // The valuation's rate date must not leak into the three tooltips that
      // it says nothing about.
      for (const testId of [
        "position-cost",
        "position-settled",
        "position-total",
        "position-profit-amount",
      ]) {
        expect(screen.getByTestId(testId).getAttribute("title")).not.toMatch(
          /19\.07\.2026/,
        );
      }
      // Tooltips only — no date ever becomes cell text.
      expect(screen.queryByText(/19\.07\.2026/)).not.toBeInTheDocument();
    });

    it("never calls the missing figure the account's, whichever gap fired", () => {
      // Position rows never say "the account's currency". The four named
      // sentences name no currency now; only the general fallback does, and it
      // must name the right one.
      for (const gap of [
        "undated_lot",
        "no_rate_lot_date",
        "no_rate_income_date",
        "no_rate_today",
      ] as const) {
        cleanup();
        wrap(
          <PositionsTable
            positions={[
              makePosition({
                currency: "USD",
                in_base: null,
                in_base_gap: gap,
              }),
            ]}
            mode="base"
            baseCurrency="RUB"
          />,
        );

        const title =
          screen
            .getByTestId("position-cost-not-converted")
            .getAttribute("title") ?? "";
        expect(title).not.toContain("счёт");
        expect(title).not.toContain("счет");
      }

      cleanup();
      wrap(
        <PositionsTable
          positions={[
            makePosition({ currency: "USD", in_base: null, in_base_gap: null }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );
      const generalTitle =
        screen
          .getByTestId("position-cost-not-converted")
          .getAttribute("title") ?? "";
      expect(generalTitle).toContain("в исходной валюте");
      expect(generalTitle).not.toContain("счёт");
      expect(generalTitle).not.toContain("счет");
    });

    it("shows a valuation in a third currency honestly, in its own currency, when in_base cannot express it", () => {
      // A bond's EUR face value on a USD position with no EUR rate: the valuation
      // has no base figure (converting with the USD rate would be wrong), while
      // cost and income convert.
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
              // The valuation's own cause from the server; in_base_gap stays null.
              market_value_gap: "no_rate_valuation_currency",
              in_base: {
                cost_minor: 9_000_000,
                market_value_minor: null,
                settled_minor: null,
                total_minor: null,
                unrealized_pnl_minor: null,
                income_minor: 0,
                currency: "RUB",
                // No single-rate figure in this object.
                rate_on: null,
              },
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      // Cost still converted and still says so.
      expect(norm(screen.getByTestId("position-cost").textContent ?? "")).toBe(
        norm(formatMinor(9_000_000, "RUB")),
      );
      expect(screen.getByTestId("position-cost")).toHaveAttribute(
        "title",
        "Пересчитано по курсам на даты покупок, а не по текущему",
      );
      // The valuation stays 1 000,00 € with a marker, never 90 000,00 ₽.
      const marketValue = screen.getByTestId("position-market-value");
      expect(norm(marketValue.textContent ?? "")).toContain(
        norm(formatMinor(100_000, "EUR")),
      );
      expect(marketValue.textContent).not.toMatch(/₽/);
      // #42: the marker names the missing EUR->USD link for this one figure,
      // not "no rate" for the whole row, which the converted cost disproves.
      const valuationMarker = screen.getByTestId(
        "position-market-value-not-converted",
      );
      expect(valuationMarker).toHaveAttribute(
        "title",
        CAPTION.valuationCurrency,
      );
      expect(valuationMarker.getAttribute("title")).not.toBe(CAPTION.general);
      // Profit is derived from that valuation, so it stays an honest dash.
      expect(screen.getByTestId("position-profit-dash")).toHaveTextContent("—");
      expect(
        screen.queryByTestId("position-profit-amount"),
      ).not.toBeInTheDocument();
    });

    it("keeps the row's own reason on the cells the valuation's reason is not true of", () => {
      // Both causes set and both true of different cells: the valuation stuck in
      // a third currency, the rest stopped by a dateless lot.
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
      expect(
        screen.getByTestId("position-market-value-not-converted"),
      ).toHaveAttribute("title", CAPTION.valuationCurrency);
      // ...and does not leak onto the cells it says nothing about.
      expect(screen.getByTestId("position-cost-not-converted")).toHaveAttribute(
        "title",
        CAPTION.undatedLot,
      );
      expect(
        screen.getByTestId("position-settled-not-converted"),
      ).toHaveAttribute("title", CAPTION.undatedLot);
    });

    it("gives the valuation the row's own cause when the valuation itself converted fine", () => {
      // in_base_gap set, market_value_gap null: the whole object was withheld,
      // so the valuation shows the row's sentence, not its own or a bare
      // «нет курса».
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

      const valuationMarker = screen.getByTestId(
        "position-market-value-not-converted",
      );
      expect(valuationMarker).toHaveAttribute("title", CAPTION.noRateLotDate);
      expect(valuationMarker.getAttribute("title")).not.toBe(
        CAPTION.valuationCurrency,
      );
      expect(valuationMarker.getAttribute("title")).not.toBe(CAPTION.general);
      expect(valuationMarker.getAttribute("title")).not.toBe(
        CAPTION.noRateToday,
      );
    });

    it("captions the valuation from market_value_gap, never from the two currency codes", () => {
      // An impossible row (currencies differ, no valuation gap) proving the
      // valuation's caption reads only the server's field, never compares the
      // codes.
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

      const valuationMarker = screen.getByTestId(
        "position-market-value-not-converted",
      );
      expect(valuationMarker).toHaveAttribute(
        "title",
        CAPTION.noRateIncomeDate,
      );
      expect(valuationMarker.getAttribute("title")).not.toBe(
        CAPTION.valuationCurrency,
      );
    });

    it("falls back to the row's cause for a valuation gap this build cannot name", () => {
      // An unknown valuation cause falls back to the row's named cause, or to
      // the general phrase when there is none.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              instrument: {
                ...makePosition().instrument,
                id: "instr-row-named",
              },
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
              instrument: {
                ...makePosition().instrument,
                id: "instr-row-unnamed",
              },
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
      // A valuation already in the base currency carries no marker, so the
      // «третья валюта» sentence never shows there; pinned so a change to either
      // half fails.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              cost_minor: 100_000,
              market_value_minor: 9_000_000,
              settled_minor: null,
              total_minor: null,
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
      expect(norm(marketValue.textContent ?? "")).toContain(
        norm(formatMinor(9_000_000, "RUB")),
      );
      expect(
        screen.queryByTestId("position-market-value-not-converted"),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByTitle(/в другой валюте, чем позиция/),
      ).not.toBeInTheDocument();
      // Cells in the position's currency keep the row's reason.
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
      // The percentage names its currency: "the share rose" versus "my base
      // currency grew".
      const position = makePosition({
        currency: "USD",
        cost_minor: 250_000,
        unrealized_pnl_minor: 25_000,
        in_base: {
          cost_minor: 2_000_000,
          market_value_minor: 2_200_000,
          settled_minor: null,
          total_minor: null,
          unrealized_pnl_minor: 200_000,
          income_minor: 0,
          currency: "RUB",
          rate_on: "2026-07-20",
        },
      });

      const { rerender } = wrap(
        <PositionsTable
          positions={[position]}
          mode="native"
          baseCurrency="RUB"
        />,
      );
      expect(screen.getByTestId("position-profit-percent")).toHaveAttribute(
        "title",
        "Доходность в USD. В другой валюте ответ другой — вплоть до противоположного знака: это два разных вопроса, а не расхождение",
      );

      rerender(
        <PositionsTable
          positions={[position]}
          mode="base"
          baseCurrency="RUB"
        />,
      );
      expect(screen.getByTestId("position-profit-percent")).toHaveAttribute(
        "title",
        "Доходность в RUB. В другой валюте ответ другой — вплоть до противоположного знака: это два разных вопроса, а не расхождение",
      );
    });

    it("names the currency the figures are actually in, not the one the mode asked for", () => {
      // Base mode without a rate keeps both figures in USD; the label follows
      // the numbers, not the mode.
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
      // Not a bug: the owner's decision (2026-07-29) is that base-currency return
      // includes the currency's move, so the two can differ in sign. Numbers from
      // TestPositionInBaseProfitInPositionCurrencyLossInBase: 10 @ $100 at 100 ₽/$,
      // $110 today at 50 ₽/$.
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
          settled_minor: null,
          total_minor: null,
          unrealized_pnl_minor: -4_500_000,
          income_minor: 0,
          currency: "RUB",
          rate_on: "2026-07-20",
        },
      });

      const { rerender } = wrap(
        <PositionsTable
          positions={[position]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      const nativeAmount = screen.getByTestId("position-profit-amount");
      expect(norm(nativeAmount.textContent ?? "")).toBe(
        norm(formatMinor(10_000, "USD")),
      );
      expect(nativeAmount.className).toContain("text-emerald-500");
      expect(
        norm(screen.getByTestId("position-profit-percent").textContent ?? ""),
      ).toBe(norm("+10,0 %"));

      rerender(
        <PositionsTable
          positions={[position]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      const baseAmount = screen.getByTestId("position-profit-amount");
      expect(norm(baseAmount.textContent ?? "")).toBe(
        norm(formatMinor(-4_500_000, "RUB")),
      );
      expect(baseAmount.className).toContain("text-red-500");
      expect(baseAmount.className).not.toContain("text-emerald-500");
      expect(
        norm(screen.getByTestId("position-profit-percent").textContent ?? ""),
      ).toBe(norm("-45,0 %"));
    });
  });

  // Income in another currency: income_minor is only the position currency's
  // part, so a yuan bond paid in roubles shows 0 there; these tests pin what the
  // column shows across the four shapes.
  describe("income arriving in another currency than the position's", () => {
    // Spelled out like CAPTION. «В сумму выше входит доход», because the cell
    // above is «Зафиксировано», realized plus income.
    const OTHER_CURRENCY_HINT =
      "Доход, пришедший не в валюте позиции: каждая сумма — в своей валюте. С суммой выше она не складывается и её пересчётом не является — это разные деньги. В сумму выше входит доход только в валюте позиции, и ноль в ней не значит, что дохода не было. По юаневой облигации купон может прийти рублями, по долларовой акции — дивиденд рублями: у российских брокеров это обычное дело. Отрицательная сумма — тоже ответ: удержанный налог вычитается в своей валюте, и если выплат в ней не было, остаётся минус";

    it("draws the ruble coupons of a yuan bond instead of leaving a bare 0 ¥", () => {
      // The live case from the owner's own account: bought for yuan, paid in
      // rubles, so income_minor is 0 and income_by_currency holds the rubles.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "CNY",
              income_minor: 0,
              income_by_currency: [{ currency: "RUB", income_minor: 135_075 }],
            }),
          ]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      expect(
        norm(screen.getByTestId("position-settled").textContent ?? ""),
      ).toBe(norm(formatMinor(0, "CNY")));
      const other = screen.getByTestId("position-income-other-currency");
      expect(norm(other.textContent ?? "")).toBe(
        norm(`ещё ${formatMinor(135_075, "RUB")}`),
      );
      expect(other).toHaveAttribute("title", OTHER_CURRENCY_HINT);
      // Never welded: the yuan cell stays 0,00 ¥ and the kopecks keep the
      // rouble sign.
      expect(norm(other.textContent ?? "")).not.toContain(
        norm(formatMinor(135_075, "CNY")),
      );
    });

    it("lists two foreign currencies side by side, in the order the server sent them", () => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "CNY",
              income_minor: 500,
              income_by_currency: [
                { currency: "CNY", income_minor: 500 },
                { currency: "RUB", income_minor: 135_075 },
                { currency: "USD", income_minor: 4_200 },
              ],
            }),
          ]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      expect(
        norm(screen.getByTestId("position-settled").textContent ?? ""),
      ).toBe(norm(formatMinor(500, "CNY")));
      // The position's own currency is not repeated below the figure that
      // already carries it, and the other two keep the server's order.
      expect(
        norm(
          screen.getByTestId("position-income-other-currency").textContent ??
            "",
        ),
      ).toBe(
        norm(
          `ещё ${formatMinor(135_075, "RUB")} · ${formatMinor(4_200, "USD")}`,
        ),
      );
    });

    it("says nothing extra when every payment arrived in the position's own currency", () => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              income_minor: 5_000,
              income_by_currency: [{ currency: "USD", income_minor: 5_000 }],
            }),
          ]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      expect(
        norm(screen.getByTestId("position-settled").textContent ?? ""),
      ).toBe(norm(formatMinor(5_000, "USD")));
      expect(screen.queryByTestId("position-income-other-currency")).toBeNull();
    });

    it("says nothing extra when the position was never paid anything", () => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              income_minor: 0,
              income_by_currency: [],
            }),
          ]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      expect(
        norm(screen.getByTestId("position-settled").textContent ?? ""),
      ).toBe(norm(formatMinor(0, "USD")));
      expect(screen.queryByTestId("position-income-other-currency")).toBeNull();
    });

    it("drops the second line once the base-currency figure — which already contains it — is what is shown", () => {
      // The converted income already includes every payment, so the roubles
      // are not repeated: 450 000 is the yuan part, 135 075 the roubles.
      const position = makePosition({
        currency: "CNY",
        income_minor: 500,
        income_by_currency: [
          { currency: "CNY", income_minor: 500 },
          { currency: "RUB", income_minor: 135_075 },
        ],
        in_base: {
          cost_minor: 2_000_000,
          market_value_minor: 2_200_000,
          settled_minor: 141_075,
          total_minor: 341_075,
          unrealized_pnl_minor: 200_000,
          income_minor: 141_075,
          currency: "RUB",
          rate_on: "2026-07-20",
        },
      });

      const { rerender } = wrap(
        <PositionsTable
          positions={[position]}
          mode="native"
          baseCurrency="RUB"
        />,
      );
      expect(screen.getByTestId("position-income-other-currency")).toBeTruthy();

      rerender(
        <PositionsTable
          positions={[position]}
          mode="base"
          baseCurrency="RUB"
        />,
      );
      expect(
        norm(screen.getByTestId("position-settled").textContent ?? ""),
      ).toBe(norm(formatMinor(141_075, "RUB")));
      expect(screen.queryByTestId("position-income-other-currency")).toBeNull();
    });

    it("keeps the second line in base mode when the position's own currency IS the base one", () => {
      // A rouble paper paid a dollar dividend: no in_base at all, so the line is
      // decided by what the cell shows, not by the mode.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "RUB",
              income_minor: 0,
              income_by_currency: [{ currency: "USD", income_minor: 5_000 }],
              in_base: null,
              in_base_gap: null,
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      expect(
        norm(screen.getByTestId("position-settled").textContent ?? ""),
      ).toBe(norm(formatMinor(0, "RUB")));
      expect(
        norm(
          screen.getByTestId("position-income-other-currency").textContent ??
            "",
        ),
      ).toBe(norm(`ещё ${formatMinor(5_000, "USD")}`));
      // Nothing was withheld, so no marker.
      expect(screen.queryByTestId("position-settled-not-converted")).toBeNull();
    });

    it("keeps the second line in base mode when the conversion could not be struck", () => {
      // Base mode without a block: the yuan figure again, with the coupons
      // line.
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "CNY",
              income_minor: 0,
              income_by_currency: [{ currency: "RUB", income_minor: 135_075 }],
              in_base: null,
              in_base_gap: "no_rate_lot_date",
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      expect(norm(visibleText(screen.getByTestId("position-settled")))).toBe(
        norm(formatMinor(0, "CNY")),
      );
      expect(
        norm(
          screen.getByTestId("position-income-other-currency").textContent ??
            "",
        ),
      ).toBe(norm(`ещё ${formatMinor(135_075, "RUB")}`));
      expect(
        screen.getByTestId("position-settled-not-converted"),
      ).toHaveAttribute("title", CAPTION.noRateLotDate);
    });
  });
  // #31: every hint about a missing figure lived in a title on a non-focusable
  // span, which screen readers need not announce. Each test asserts both halves
  // (announcedText in test-utils): the eye gets the dash, the ear the
  // sentence.
  describe("a missing figure explains itself to a reader who is not looking", () => {
    it("announces the valuation cell's reason instead of a bare dash", () => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              market_value_minor: null,
              market_value_currency: null,
              unrealized_pnl_minor: null,
              market_value_gap: "no_quote",
            }),
          ]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      const dash = screen.getByTestId("position-no-quote");
      expect(visibleText(dash)).toBe("—");
      expect(announcedText(dash)).toBe(NO_VALUATION.noQuote);
    });

    it("announces both of the profit cell's sentences, run together as speech and not as markup", () => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              market_value_minor: null,
              market_value_currency: null,
              unrealized_pnl_minor: null,
              market_value_gap: "no_quote",
            }),
          ]}
          mode="native"
          baseCurrency="RUB"
        />,
      );

      const dash = screen.getByTestId("position-profit-dash");
      expect(visibleText(dash)).toBe("—");
      // One string, its line break read as a space; the tooltip keeps it.
      expect(announcedText(dash)).toBe(
        `${PROFIT_NEEDS_VALUATION} ${NO_VALUATION.noQuote}`,
      );
      expect(dash).toHaveAttribute(
        "title",
        `${PROFIT_NEEDS_VALUATION}\n${NO_VALUATION.noQuote}`,
      );
    });

    it("announces why a figure is shown in its own currency rather than the base one", () => {
      wrap(
        <PositionsTable
          positions={[
            makePosition({
              currency: "USD",
              cost_minor: 100_00,
              in_base: null,
              in_base_gap: "no_rate_lot_date",
            }),
          ]}
          mode="base"
          baseCurrency="RUB"
        />,
      );

      // The icon has no text of its own.
      const indicator = screen.getByTestId("position-cost-not-converted");
      expect(visibleText(indicator)).toBe("");
      expect(announcedText(indicator)).toBe(CAPTION.noRateLotDate);
    });
  });
});

// Р-11: the full valuation above says where its price came from, and the
// liquid one — what can be sold now — is said beneath it when it differs.
describe("PositionsTable — the two valuations", () => {
  const show = (overrides: Partial<Position>) =>
    wrap(<PositionsTable positions={[makePosition({ price_on: daysAgo(2), ...overrides })]} mode="native" baseCurrency="RUB" />);

  it("says nothing more for a paper the market prices", () => {
    show({});
    expect(screen.queryByTestId("position-liquid")).toBeNull();
    expect(screen.queryByTestId("position-not-traded")).toBeNull();
    expect(screen.queryByTestId("position-price-nav")).toBeNull();
  });

  it("names a fund's net asset value and what the units sell for now", () => {
    show({ price_source: "nav", liquid_value_minor: 65_00 });
    expect(screen.getByTestId("position-price-nav").textContent).toBe("по стоимости чистых активов фонда");
    expect(norm(screen.getByTestId("position-liquid").textContent ?? "")).toBe(
      norm(`продать сейчас: ${formatMinor(65_00, "USD")}`),
    );
  });

  it("names a foreign share's home exchange", () => {
    show({ price_source: "foreign", liquid_value_minor: 300_00 });
    expect(screen.getByTestId("position-price-foreign").textContent).toBe("по цене на бирже эмитента");
  });

  it("says a paper does not trade, and since when", () => {
    show({ liquid_value_minor: null, last_traded_on: "2022-02-25" });
    expect(screen.getByTestId("position-not-traded").textContent).toBe(
      "не торгуется с 25.02.2022 — в «Итого» не входит",
    );
  });
});
