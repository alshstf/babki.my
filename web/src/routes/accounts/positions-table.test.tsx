import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import "@/i18n";
import { PositionsTable } from "./positions-table";
import type { CashPosition, Position } from "@/api/positions";
import { formatMinor } from "@/lib/money";
import { announcedText, visibleText } from "@/test-utils";

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
  // The one sentence here that is not the server's answer but the absence of
  // one, and #105 is about what it may therefore claim. It used to open «Нет
  // курса», which names a RATE as the cause — and this build reaches it exactly
  // when it does not know the cause: the value off the wire is outside the
  // union it was compiled against. `undated_lot` is already in today's enum and
  // is not about a rate at all, so the next date-shaped cause the server adds
  // would be captioned «нет курса» by every client one release behind — the
  // very defect (#66) the named sentences above exist to end, coming back in
  // through the path built to degrade safely. The replacement states only what
  // the payload itself shows: the base-currency figures were withheld.
  general:
    "В базовой валюте эта позиция не посчиталась, а причина не названа. Поэтому числа этой строки показаны в исходной валюте",
  // #66's wording fix: the old ending («строка показывается в базовой валюте
  // целиком или никак») stated a rule about how EVERY row displays, and a row
  // with in_base present plus its own market_value_gap breaks it (cost/income
  // in rubles, valuation in euros, on the same row). The new ending is scoped
  // to the row this caption sits on and stays true regardless of what a
  // different row is doing.
  undatedLot:
    "У одной из партий не записана дата покупки, а стоимость считается по курсу на день покупки — и восстановить эту дату уже неоткуда: в базовой валюте эта позиция сама не посчитается. Поэтому ни одно число этой строки не показано в базовой валюте",
  noRateLotDate:
    "Нет курса на день покупки одной из партий, а стоимость считается по курсу того дня. Если курс появится при обновлении курсов, позиция посчитается сама. Поэтому пока ни одно число этой строки не показано в базовой валюте",
  // These two are the only sentences on the screen that say anything about a
  // step that did NOT fail, and they are worded around what the server
  // actually did. It stops at the first term it cannot value, so reaching the
  // income means the cost did not stop it — which is not the same as "the
  // purchase-day rates were found": a position that holds no lot has no
  // purchase day to look one up for, and a closed row that took a dividend
  // reaches `no_rate_income_date` having asked the rate table for nothing at
  // all. «дело не в стоимости» is true whether that sum had terms or none;
  // «курсы на дни покупок нашлись» was not, and was one more instance of the
  // very defect this branch exists to end.
  noRateIncomeDate:
    "Дело не в стоимости: нет курса на день одной из выплат — дивиденда, купона или налога, — а доход считается по курсу того дня. Если курс появится при обновлении курсов, позиция посчитается сама. Поэтому пока ни одно число этой строки не показано в базовой валюте",
  noRateToday:
    "Дело не в стоимости и не в доходе: нет курса на сегодня — для валюты, в которой считается рыночная оценка, — а оценка берётся по текущему курсу. Если курс появится при обновлении курсов, позиция посчитается сама. Поэтому пока ни одно число этой строки не показано в базовой валюте",
  valuationCurrency:
    "Оценка получилась в другой валюте, чем позиция, а курса от неё до валюты позиции нет: сравнить её со стоимостью позиции нельзя. Пока оценка не выражена в валюте позиции, программа не показывает её и в базовой. Поэтому показана в исходной валюте",
} as const;

// The sentences for an EMPTY valuation cell, one per value of
// Position.market_value_gap that says no valuation was struck, plus the general
// one for a cause this build cannot name. Spelled out in full for the same
// reason CAPTION is: which sentence lands on the dash is the whole of #78, and
// a test reading them back through the component's own lookup would agree with
// whatever it picked.
//
// The three named ones are three different pieces of news and are worded to be
// unmistakable for one another. Only ONE of them mentions a quote as the thing
// that is missing, and it is the only one where a quote IS the thing that is
// missing. The other two are about a row that may well have a perfectly good
// quote — and neither of them may claim that it HAS one either: the server
// reports both of them whether a quote exists or not, because the ordering rule
// puts the cause a quote would not close first (see MarketValueGap in the API
// contract).
const NO_VALUATION = {
  noQuote:
    "Котировки пока нет. Если она появится, рыночная оценка посчитается сама",
  typeNotPriced:
    "Рыночную оценку для этого вида активов программа не считает — такого расчёта в ней нет. Котировка тут ничего не меняет: даже когда она есть, оценки не будет, и ждать её не нужно",
  noFaceValue:
    "У этой облигации не записан номинал, а котируется она в процентах от номинала: брать процент не от чего. Котировка тут ничего не меняет — пока номинала нет, оценки не будет",
  general: "Рыночной оценки нет, а причина не названа",
} as const;

// What the PROFIT dash adds in front of the valuation's sentence. The profit
// column's own dash needs its own first line: the sentences above explain why
// there is no valuation, and this is why that leaves the profit cell empty
// too.
const PROFIT_NEEDS_VALUATION =
  "Прибыль — это рыночная оценка минус стоимость, а оценки нет";

// The two sentences the price line's tooltip carries under the date, spelled
// out here in full for the same reason the captions above are: what this is
// about is WHICH sentence sits beside the number, and a test that fetched it
// through the component's own lookup would agree with the component whatever
// it picked.
//
// WHY THEY EXIST. price_on is the trading session the source itself attaches
// the price to — never the day this program fetched it, which is what #90 was
// (the fetch day stored as the price's own, so Friday's price read «Цена на
// 03.08.2026» on the Monday). With the date now true, a bare «Цена на
// 31.07.2026» read on a Monday is still two different pieces of news wearing
// one sentence — "this is the market's last word" and "the quote job stopped
// updating" — and nothing else on this screen tells them apart. So the caption
// says what the date IS rather than leaving the reader to guess.
//
// WHAT THEY MUST NOT SAY is the other half of the same care, and it is why
// these two sentences are worded so carefully around the exchange's own
// behaviour (all of it measured against live ISS — see the QuotesFor doc block
// in internal/marketdata/moex/moex.go):
//   - not «цена закрытия». MOEX publishes the official close in a different
//     column (PREVLEGALCLOSEPRICE) and it is a different number — SBER's two
//     read 276.52 and 275.60 on one row.
//   - not "traded at that price on that day". Of TQCB's 3021 rows measured for
//     one session, 779 carried a price for a paper that did not trade at all,
//     and RU000A103AP6 reported a price last struck three weeks earlier beside
//     that session's date.
//   - not «предыдущая сессия». The wire carries whatever day the SOURCE named
//     (TickerQuote.On), and this screen knows nothing of any source's habits;
//     "previous" is a fact about MOEX's PREVPRICE, not about the contract.
//   - not "so the data is fresh". A date in the past is exactly what a broken
//     quotes job looks like too, and a caption that reassured the reader would
//     be covering for it.
const PRICE_SESSION_NOTE =
  "Это цена той торговой сессии: так её датирует источник котировки, а не программа в день загрузки. Сделки в тот день могло и не быть — источник называет цену и для бумаги, которая не торговалась";
// The second half of the picture, and the project's "three rates for three
// questions" rule applied to this cell: the valuation is struck from a price
// belonging to some past session, but any conversion of that valuation is done
// at the CURRENT rate (internal/portfolio/http.go converts into the position's
// currency and into the base one at `now`, never at the quote's date). The
// conditional wording is load-bearing — a position whose valuation is already
// in its own currency, shown in native mode, converts nothing at all.
const PRICE_VALUATION_NOTE =
  "Рыночная оценка посчитана из этой цены. Если оценку пересчитывают в другую валюту, берётся текущий курс, а не курс на эту дату";

// Every money cell of a row carries the row's caption; the valuation is the
// one that can carry its own instead (market_value_gap). Named here so a test
// can say "all four" without repeating the ids.
const ROW_MARKERS = [
  "position-cost-not-converted",
  "position-market-value-not-converted",
  "position-profit-amount-not-converted",
  "position-settled-not-converted",
] as const;

// makePosition builds a row the way the SERVER would, which for the two summed
// figures means deriving them rather than restating them: settled is the
// realized result plus the income, and total adds the unrealized half (see
// Position.settled_minor in the API contract). A fixture that made a caller
// spell all four out would let a test assert an income of 5,00 beside a settled
// of nought and never notice.
//
// Overriding either one explicitly still wins — that is how the null cases are
// written, and they are the cases the derivation cannot produce.
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
    // The ordinary row: nothing was ever paid, so the per-currency list is
    // empty and income_minor's 0 is the whole story. A row whose income
    // arrived in another currency sets both fields, and the two must agree —
    // income_minor is defined as this list's entry for `currency` (see
    // Position.income_minor in the API contract), so a fixture that set one
    // without the other would describe a payload the server cannot send.
    income_by_currency: [],
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
    wrap(
      <PositionsTable
        positions={[makePosition()]}
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
    // ...but the date is not — it moved into the title tooltip.
    expect(screen.queryByText(/20\.07\.2026/)).not.toBeInTheDocument();
    expect(priceLine.getAttribute("title")).toContain("Цена на 20.07.2026");
  });

  it("captions the price with the session it belongs to, not with the day it was fetched", () => {
    wrap(
      <PositionsTable
        positions={[makePosition()]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    // Pinned as ONE exact string, in order: swapping any of the three lines
    // for another sentence, dropping one, or reordering them fails here. That
    // is the whole point of this test — the number was never the thing at
    // risk, the sentence beside it was (see PRICE_SESSION_NOTE above for what
    // it is forbidden to say).
    expect(screen.getByTestId("position-price").getAttribute("title")).toBe(
      `Цена на 20.07.2026\n${PRICE_SESSION_NOTE}\n${PRICE_VALUATION_NOTE}`,
    );
  });

  it("prints every converted figure of a row in the currency that row's in_base carries, not the session's", () => {
    // #106, in the shape the owner meets it: settings changes the base
    // currency, the session's new answer lands in the cache at once
    // (useUpdateSpace writes it directly), and this screen still holds figures
    // the server converted into the OLD base currency until its refetch comes
    // back. The rubles must keep printing as rubles for that window — a euro
    // sign over them is not a mislabelling, it is a number wrong by the whole
    // exchange rate with nothing on screen admitting it.
    //
    // All four money cells of the row are checked, because all four read the
    // one block and each could have been left reading the session instead.
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
              // Deliberately NOT the quote's date: the two answer different
              // questions ("which session is this price of" vs "which day's
              // rate converted the valuation"), and a price line that read
              // this field instead would still print a plausible date.
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
            // Non-zero income so the "no fake 0,00" assertion below can
            // only be satisfied by the market value column behaving —
            // income legitimately being 0 would otherwise make that check
            // vacuous. (Realized/fees are no longer rendered at all.)
            income_minor: 500,
            market_value_minor: null,
            market_value_currency: null,
            price: null,
            price_on: null,
            // The contract publishes a cause on every row with no valuation
            // (#78), and this fixture is the one the phrase «нет котировки» is
            // actually true of: an ordinary share whose price has not arrived.
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
    // "not preceded by a digit" excludes legitimate non-zero amounts that
    // happen to end in "0,00" (e.g. "500,00"), while still catching a real
    // fake-zero amount ("0,00").
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
    // The profit is missing BECAUSE the valuation is, so this cell says that
    // and then hands over to the valuation's own reason. It used to print
    // «Нет котировки» flat, which on two of the three kinds of row that reach
    // here is false (see the describe block below).
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
    // Issue #78. Three different things leave a position with no market
    // valuation, and this screen used to put «Нет котировки» over all three.
    // On two of them a quote exists and is not what is missing — the program
    // has no valuation model for the instrument's type, or the bond's face
    // value was never recorded — so the reader was sent off to wait for data
    // that was already there. Only the server knows which of the three
    // happened, and since #78 it says so in Position.market_value_gap.
    //
    // Every test here pins the EXACT sentence and, where it matters, asserts
    // that the other ones are not the one shown: a caption that is merely
    // different is not the property under test, the right cause is.
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
      // The two things this sentence is forbidden to do, both of them the
      // reason the value exists at all. It may not send the reader off after a
      // quote — the row may already have one, and a new one changes nothing —
      // and it may not promise the figure is coming, because no decision to
      // write such a valuation has been taken.
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
      // The #105 rule on this cell: a server NEWER than this build sends a
      // value outside the union, and an OLDER one sends null where the
      // contract now requires a cause. Neither may be captioned «Нет
      // котировки» — that is a claim about a quote, and the condition that
      // brings us here is not knowing anything about one.
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
      // The second cell #78 lands on. The profit is a dash because the
      // valuation is, so the reason for one is the reason for the other — and
      // this cell said «Нет котировки» over a crypto row just as the valuation
      // cell did.
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
      // The other half of the profit dash, unchanged and asserted here beside
      // its neighbour: with a valuation present the profit is missing for a
      // different reason entirely, and the valuation's absence sentences say
      // nothing true about it.
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

  // A bond is quoted as a PERCENTAGE of its face value (95.20 meaning 95.20 %
  // of the face value), and that percentage is what the server publishes in
  // Position.price — see marketValue() in internal/portfolio/http.go, where a
  // bond's valuation is faceValueMinor × price/100 × quantity. The demo seed's
  // ОФЗ 26238 is exactly this: face value 1 000,00 ₽, quote 95.20, so the money
  // one bond is worth is 952 ₽ and the figure under the valuation is 95,20.
  // Printed bare beside a ruble amount that reading is off by a factor of ten
  // (#32), so the unit is stated. What is NOT done is deriving the 952 ₽:
  // that is money arithmetic in the browser, which this project does not do.
  //
  // The caption states the RULE (face × percent) rather than pointing at "the
  // figure above" or naming a derived amount: the cell above is only that
  // product when the row is shown in the position's own currency. In base
  // mode it is the base-currency valuation instead, which is not face ×
  // percent × quantity in any currency — a caption that pointed at it would
  // be false in exactly the mode the owner uses.
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
      // 1 000,00 ₽ of face at 95.20 % = 952,00 ₽ for one bond. The SERVER
      // strikes this (Position.price_money_minor) — the screen never multiplies
      // the face value itself — so a fixture that omitted it would be testing
      // the percent-only row, which is the case below.
      price_money_minor: 95_200,
      ...overrides,
    });
  }

  // THE THREE FIGURES THE OWNER ASKED FOR, AND WHAT SEPARATES THEM. «Прибыль»
  // is the unrealized half alone — what the paper is worth today against what
  // it cost. «Зафиксировано» is the settled half: the result of closed deals
  // plus what the paper has paid out, money that is already his. «Всего» is
  // both, which is what he was reaching for a calculator to work out.
  //
  // Three DIFFERENT numbers in one fixture on purpose: a cell wired to the
  // wrong one of the three still prints a plausible amount, and only figures
  // that disagree can catch it.
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
    // A closed position's unrealized profit is honestly nothing, and the column
    // that says «Прибыль» must keep saying nothing — while the money the sale
    // actually made gets its own line under it rather than being folded into
    // that cell under the wrong word.
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
    // The cell shows one number and the tooltip shows what it is made of —
    // which is where the «Доход» column went. Pinned as one exact string
    // because two of its claims are claims about the SERVER: that the tax on a
    // dividend is already inside the income (Position.income_minor subtracts
    // it), and that the tax withheld from the ACCOUNT is not
    // (RealizedTotal.tax_withheld_by_currency carries that one, under the
    // table). An earlier wording said flatly «налог тут не вычтен», which was
    // false of every dividend this program has ever recorded.
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
        "Свершившееся, которое больше не изменится: реализованная прибыль 100,00 $ плюс доход 10,00 $. Налог с дивиденда или купона уже вычтен из дохода. А тот, что брокер списывает со счёта при выводе средств, тут не вычтен — он берётся с накопленной за год базы, а не с бумаги, и показан общей суммой под таблицей",
      ),
    );
  });

  // THE DASH UNDER «ЗАФИКСИРОВАНО» HAS TWO CAUSES AND MUST NAME THE RIGHT ONE.
  // The server withholds the figure when a disposal settled in a currency the
  // position is not denominated in, and also when the payments arrived in more
  // than one currency — unrelated events that a single sentence would describe
  // wrongly half the time.
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
    // A yuan bond paying its coupons in rubles: nothing was sold, the realized
    // figure is a perfectly good nought, and the old single caption would have
    // announced a disposal that never happened.
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

  // MONEY IS A HOLDING, AND THESE ROWS ARE WHERE IT BECAME ONE. Yuan on a
  // Russian broker's account was bought at one rate and is worth another today;
  // before this the only cash figure on the screen was a snapshot the broker
  // named, in rubles, with no cost behind it.
  function makeCash(overrides: Partial<CashPosition> = {}): CashPosition {
    return {
      currency: "USD",
      amount_minor: 150_000,
      in_base: {
        currency: "RUB",
        value_minor: 13_500_000,
        cost_minor: 9_000_000,
        unrealized_pnl_minor: 4_500_000,
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
    // A thousand dollars cost a thousand dollars and is worth a thousand
    // dollars. Printing that figure three times across the row would look like
    // three answers where there is one.
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

  it("says nothing about a profit on the base currency itself", () => {
    // Rubles in a ruble space cost rubles and are worth rubles. The server does
    // publish an honest nought here; the row shows the balance and no more,
    // because a column of noughts across every account is noise.
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

  it("shows a negative balance rather than hiding it", () => {
    // The owner's own case: the journal spends yuan whose purchase the broker
    // would not explain, so the balance goes below nought. That IS the
    // discrepancy, and a row that hid it would agree with a broker it does not
    // agree with.
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
  });

  it("hides a currency the account holds nothing of, behind the same control", () => {
    // An account that bought dollars and sold them all again has held dollars.
    // The row is not noise — it is where that history lives — but it is not
    // today's portfolio either, so it sits behind the control that shows what
    // is no longer held.
    wrap(
      <PositionsTable
        positions={[makePosition(), makeClosed()]}
        cash={[makeCash({ amount_minor: 0 })]}
        mode="native"
        baseCurrency="RUB"
      />,
    );

    expect(screen.queryByTestId("cash-row")).not.toBeInTheDocument();
    // ...and the control's count is about PAPERS: an empty balance is not a
    // closed position, and counting it would make the sentence wrong.
    expect(screen.getByTestId("closed-positions-note").textContent).toContain(
      "1",
    );

    fireEvent.click(screen.getByTestId("toggle-closed-positions"));
    expect(screen.getByTestId("cash-row")).toBeInTheDocument();
  });

  // A POSITION SOLD OUT OF IS STILL HISTORY, AND STILL OFF THE SCREEN BY
  // DEFAULT. Both halves are the point: the row keeps a realized result and the
  // income the paper paid, so deleting it would lose real answers — and a
  // portfolio of a few holdings should not open with a list of things that are
  // over. What makes hiding honest is the count beside the control, so these
  // tests check the sentence as closely as the filtering.
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
    // The number of hidden rows, and the fact that the figures under the table
    // are NOT filtered with them — a header standing over rows that show none
    // of it reads as an error rather than as the deliberate answer it is.
    expect(screen.getByTestId("closed-positions-note").textContent).toBe(
      "скрыто закрытых: 1. Итоги под таблицей считаются по всем позициям, включая скрытые",
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
      "закрытых среди них: 1",
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
    // MONEY FIRST, THE PERCENT BESIDE IT: 952,00 ₽ is what one bond costs, and
    // 95,20 % is how the market quotes it. The money is the server's figure
    // (price_money_minor) printed as given — the multiplication behind it is
    // not repeated here — and its sign belongs to the FACE VALUE's currency,
    // never to the percent, which is denominated in nothing.
    expect(norm(priceLine.textContent ?? "")).toBe("952,00 ₽ · 95,20 %");
    // norm() above strips NBSP, so it can't tell a real non-breaking space
    // from a plain one that would let "%" or "₽" wrap onto its own line — pin
    // the raw characters too, unnormalized, against ru.json's
    // priceMoneyAndPercent key. The space around "·" is an ordinary one on
    // purpose: that is where the line MAY break.
    expect(priceLine.textContent).toBe("952,00\u00a0₽ · 95,20\u00a0%");
    // The session note sits under the date it explains and ABOVE the
    // percentage note, and the valuation note under both: a reader learns what
    // the date means, then what the number is, then what was computed from it.
    // The face-value caveat comes with the money, and only with it.
    expect(norm(priceLine.getAttribute("title") ?? "")).toBe(
      norm(
        `Цена на 20.07.2026\n${PRICE_SESSION_NOTE}\n${BOND_PRICE_NOTE}\n${BOND_MONEY_NOTE}\n${PRICE_VALUATION_NOTE}`,
      ),
    );
  });

  it("prints the percentage alone, and no face-value caveat, when the server sent no money price", () => {
    // The server publishes price_money_minor only where it can strike it, and
    // the screen must not fill that gap in: multiplying the face value here
    // would be money arithmetic on the client. The percent alone is what such
    // a row actually knows — and it is still NOT a currency sign, which is the
    // other half of #76: the valuation right above IS in rubles and
    // market_value_currency says so, which is the field a share's price reads
    // its own currency from.
    //
    // The caveat about the face value drifting goes with the money and only
    // with it: here it would explain a number that is not on screen.
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

  // #76, IN THE SHAPE THE OWNER MEETS IT. The valuation cell converts with the
  // display-currency toggle and this price line does not — it is the quote,
  // published exactly as quoted — so in base mode a foreign share printed a
  // ruble amount with a bare dollar figure under it, and nothing on the row
  // said which of the two was which. #32 fixed the same shape for bonds by
  // adding «%» and left this one open, because a share's price needed a
  // currency rather than a unit.
  //
  // 10 × $305,50 = $3 055,00, which at 90 ₽/$ is the 274 950,00 ₽ above it.
  // Those two numbers are deliberately far apart: a sign that only ever
  // appeared where the figures already agreed would prove nothing.
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

  // WHICH FIELD describes the price, on the one row where the two candidates
  // disagree. A quote carries a currency of its own, and where it differs from
  // the position's the server converts the VALUATION into the position's
  // currency and discloses the original in market_value_source_currency —
  // while Position.price stays exactly as quoted (see its description in the
  // API contract). So the price's currency is the SOURCE one here, and
  // market_value_currency — the currency of the figure above — is the wrong
  // answer that would look right on every other row.
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

  // The digit rules and the currency are one rendering, not two that meet
  // later: the seed's WeWork is quoted at $0.0025 after its bankruptcy, and
  // «0,00 $» would be a fake zero wearing a currency sign — #30 undone by
  // #76's own fix.
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

  // A type this build cannot read a price for gets no currency claimed on its
  // behalf. Today's server never sends such a row — it publishes no valuation
  // and no price for anything but share, etf and bond (marketValue in
  // internal/portfolio/http.go), so `hasMarketValue` is false and this hint is
  // not rendered at all — and that is exactly why the branch is written and
  // pinned rather than folded into the share/etf one: the day a NEWER server
  // prices a new type, what its Position.price means will be decided there,
  // and a client one release behind must not answer that question with the
  // valuation's currency. It is the same rule the gap captions follow (#105):
  // what is true stays said, what is not known stays unsaid.
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
    // The row where a bond has BOTH candidate currencies filled in — the
    // position's own on the valuation, the face value's on the source — and
    // the percentage belongs to neither of them (#76). The money beside it
    // belongs to the FACE VALUE's currency, which is where the server struck
    // it, and which is why the dollar of this position is nowhere on the line.
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
      // No "not converted" indicators anywhere — every figure had a rate.
      // (Checked by test id, not by text: the marker is an icon whose
      // wording lives in a title attribute, so a text query can never see it
      // and would pass vacuously.)
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

      // The amount itself is still shown, honestly, in its own currency —
      // and it is all that is shown, the not-converted sentence being a
      // tooltip and a screen-reader-only copy of it (#31).
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

      it("makes the figure's return conditional on the rate for every closeable cause, and offers it for no other", () => {
        // The whole user value of naming the term: «подтянется само» versus
        // «само не подтянется» — the closeable causes say the figure appears
        // on its own once the rate is there, the permanent one (undated_lot)
        // says only that nothing will fix it automatically, never that it
        // can't be fixed at all. Asserted as a property of the four sentences
        // rather than case by case, so a new cause worded without either half
        // fails here.
        //
        // #105's second half is the «Если»: the rate's ARRIVAL is not this
        // program's to promise. Rates come from one source whose list of
        // currencies is its own, so a pair it publishes no leg of never gets
        // one, and «Курс появится при обновлении курсов» told such a row to
        // wait for something that is never coming. Which rows those are, the
        // client cannot know — the contract names no source and lists no
        // currencies — so it may not qualify the sentence by pair either. What
        // it can state is the consequence, and only as a consequence: IF the
        // rate turns up, the figure follows. That much this program does
        // guarantee, since every request recomputes from whatever rates exist
        // at the time.
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
        // The two sentences that say anything POSITIVE about an earlier step
        // say only that it did not stop the sum. A position holding no lot
        // still reaches no_rate_income_date on a dividend, and there the
        // server asked the rate table for no purchase day at all — «курсы на
        // дни покупок нашлись» reported a check that never ran, which is this
        // branch's own defect turned on the branch. Pinned as a property of
        // the wording, because the payload carries no evidence of which terms
        // a sum had and the caption cannot vary with them.
        for (const gap of ["no_rate_income_date", "no_rate_today"] as const) {
          const [cost] = captionsFor(gap);
          expect(cost).toContain("Дело не в стоимости");
          expect(cost).not.toContain("нашлись");
        }
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
      // A server newer than the client sends a value outside the union this
      // build knows. That must degrade to a vague-but-true sentence — not to a
      // blank tooltip, not to a thrown render, and not to one of the four
      // specific sentences, which would name a cause the server did not
      // state. The second row is the same requirement for a payload with no
      // cause at all (an older server, or one breaking its own contract).
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
      // #105, asserted as the property and not only as the string. The
      // fallback must blame nothing: not a rate, not a date, not a currency.
      // «Нет курса» is checked in the exact shape it had, because that is what
      // this sentence used to open with, and «курс» in any form is checked
      // because the point is the CAUSE named, not one phrasing of it — a
      // rate-shaped cause is precisely the one the client cannot know is the
      // right one, `undated_lot` being in today's own enum.
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
      // The settled figure is realized result plus income, and every term of it
      // is a past event with a date of its own — so the sentence names all
      // three kinds of date rather than the payments alone, which was the whole
      // of this cell back when it showed nothing but income.
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
                settled_minor: null,
                total_minor: null,
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
      expect(norm(marketValue.textContent ?? "")).toContain(
        norm(formatMinor(100_000, "EUR")),
      );
      expect(marketValue.textContent).not.toMatch(/₽/);
      // Issue #42: the marker must name the cause that IS the cause. The rate
      // that is missing is the link from EUR to the position's USD, without
      // which the row has nothing to compare the valuation against, and the
      // backend withholds the base-currency figure with it. It is missing for
      // this ONE figure: the cost in the very same row converted into rubles
      // above. "Нет курса" is the row's marker and would say the row could not
      // be converted at all, which the cell beside it disproves.
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
      // Same requirement as the row's unknown value, answered one step
      // differently: a valuation whose own cause this build cannot name is
      // still covered by whatever stopped the row, which the server did name —
      // so the cell degrades to that rather than to blankness. When the row has
      // no named cause either, that fallback is itself the general phrase.
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

  // THE INCOME A POSITION EARNED IN A CURRENCY THAT IS NOT ITS OWN.
  //
  // Position.income_minor carries the income denominated in the position's
  // currency and nothing else — the contract says so in as many words — so a
  // yuan bond whose coupons arrive in rubles, which is what a Russian broker
  // ordinarily pays, has 0 in that field. Drawn alone that zero is
  // indistinguishable from a paper that has never paid anything, and the
  // rubles were on no screen at all. These tests pin what the column shows
  // instead, over all four shapes the income can take.
  describe("income arriving in another currency than the position's", () => {
    // Spelled out in full rather than read back out of ru.json: which sentence
    // sits beside the number is the whole point, and a test that fetched it
    // through the component's own lookup would agree with the component
    // whatever it picked (same rule as CAPTION above).
    // "В сумму выше входит доход" and not "сумма выше — это доход": the cell
    // above this line is «Зафиксировано» now, which is the realized result PLUS
    // the income. The old wording named the whole cell as income and was made
    // false by the column that replaced it.
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
      // The two figures are never welded into one: the cell above stays 0,00 ¥
      // and the kopecks below keep the ruble sign. Printing them under the
      // position's own sign instead — the one-character mistake this line
      // guards — would put 135 075 kopecks behind a yuan sign and make the row
      // a hundred-and-a-bit times richer than it is.
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
      // in_base.income_minor converts EVERY payment out of the currency it
      // arrived in (see PositionInBase.income_minor in the API contract), so
      // repeating the rubles beneath it would show the same money twice and
      // invite a reader to add it to a sum that already holds it. 450 000 is
      // the yuan part converted; 135 075 the rubles that needed no rate.
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
      // The case this row could not show in either mode: a ruble paper paid a
      // dollar dividend. Nothing needs converting, so the server publishes no
      // in_base at all, and the cell shows the position's own figure — which
      // carries rubles only. Deciding by the mode instead of by what the cell
      // ended up showing would hide the dollars here and mark them with
      // nothing.
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
      // Nothing was withheld here — there was nothing to convert — so the cell
      // carries no "could not convert" marker to explain the second line
      // away.
      expect(screen.queryByTestId("position-settled-not-converted")).toBeNull();
    });

    it("keeps the second line in base mode when the conversion could not be struck", () => {
      // The fallback: base mode, no converted block, so the cell shows the
      // position's own figure again — and that figure is once more the yuan
      // part alone. The row's own caption explains why nothing is in rubles;
      // it says nothing about the coupons, which is what this line is for.
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
  // #31. Every hint on this screen about a figure that is missing or could not
  // be converted was carried by a `title` attribute alone, on a <span> that
  // holds no text of its own and that nothing can focus. That is not a hint
  // that is merely awkward to reach without a mouse: a roleless, textless span
  // is a generic container, a tooltip on one is not something assistive
  // technology is obliged to announce, and there is no keyboard route to one
  // outside the tab order either — so a reader using a screen reader met an
  // empty cell and was told nothing at all about why it was empty. The row's numbers are the whole product; a reason a number is
  // absent is part of the number.
  //
  // Each test below asserts BOTH halves — see announcedText in test-utils. The
  // pair is the property: the eye still gets a dash and not a paragraph, the
  // ear gets the sentence and not a dash. Either assertion alone passes on an
  // arrangement that fails the other, which is exactly how this defect got
  // written in the first place.
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
      // One string, read out with its line break collapsed to a space the way
      // any run of whitespace in markup is — which is the right rendering for
      // speech and needs no second copy to get it. The tooltip keeps the break
      // because a tooltip renders "\n" as a line; both come from the same
      // value, so they cannot come to say different things.
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

      // The indicator beside the number, which is an icon and therefore has no
      // text of its own to be read out — it used to carry the whole
      // explanation in a `title` and nothing else.
      const indicator = screen.getByTestId("position-cost-not-converted");
      expect(visibleText(indicator)).toBe("");
      expect(announcedText(indicator)).toBe(CAPTION.noRateLotDate);
    });
  });
});
