import { useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  formatMinor,
  formatPrice,
  formatPriceIn,
  signClass,
} from "@/lib/money";
import { formatDate } from "@/lib/dates";
import {
  resolveDisplayAmount,
  resolveOptionalDisplayAmount,
} from "@/lib/display-amount";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import { MoneyCell } from "@/components/money-cell";
import { unnameableGap } from "@/lib/unnameable-gap";
import type {
  CashPosition,
  InBaseGap,
  MarketValueGap,
  Position,
} from "@/api/positions";

// The one sentence that captions EVERY money cell of a row, chosen from the
// term the server says it stopped on (Position.in_base_gap). It is the row's
// only source: has_undated_lots answers the same question for the first of
// these four causes and cannot disagree with it — both derive from one
// server-side predicate — but a caption assembled from two sources is a
// caption that will eventually be assembled from two sources that have drifted.
//
// Each sentence explains why the WHOLE row carries no base-currency figures,
// not why one cell does, and that is what makes it true over all four cells:
// in_base is published as a whole or not at all, so a single unvaluable term
// withholds the cost, the income, the profit AND the valuation together. A
// sentence that named only its own term — «нет курса на день покупки» — would
// hang over the income and the valuation, which need no purchase date, and
// that is exactly the defect (#66) this is fixing rather than repeating.
//
// The server checks its terms in a fixed order and stops at the first failure,
// so a named cause also asserts that nothing before it stopped the object; two
// of these sentences say so out loud («дело не в стоимости»), which is what
// answers the reader's obvious question about the ruble figure they can see is
// missing from a cell whose own rates are all there. They claim no more than
// that on purpose: a term the server never had to value — the cost of a
// position holding no lot, the income of one that received none — was not
// checked and found sound, it was simply never in the way, so «курсы на дни
// покупок нашлись» over a sum with no terms reported a lookup that never
// happened.
//
// Written as a switch over literal keys rather than a lookup table, so every
// key stays a literal at the t() call site — the only shape
// scripts/check-i18n.mjs can verify.
function rowGapTitle(
  t: (key: string) => string,
  gap: InBaseGap | null,
): string {
  switch (gap) {
    case "undated_lot":
      return t("positions.notConvertedUndatedLot");
    case "no_rate_lot_date":
      return t("positions.notConvertedNoRateLotDate");
    case "no_rate_income_date":
      return t("positions.notConvertedNoRateIncomeDate");
    case "no_rate_today":
      return t("positions.notConvertedNoRateToday");
    case null:
      // The contract publishes a cause whenever in_base is null and the
      // currency differs from the base one, so a row with a marker and no
      // cause should not occur. It is not left to crash or to say nothing:
      // the general phrase says only what the payload itself shows — the
      // base-currency figures were withheld — and it is what an older
      // server's payload deserves.
      return t("positions.notConverted");
    default:
      // The other way to reach that phrase, and the one that decided how it
      // is worded (#105). A server NEWER than this build sends a value outside
      // the union above; the sentence shown then may claim nothing about the
      // cause, because not knowing the cause is the very condition that brings
      // it here. «Нет курса», which is what it used to say, claims exactly
      // that — and would be false of a value shaped like `undated_lot`, which
      // is in TODAY's enum and is about a date nobody recorded rather than
      // about a rate. So a client one release behind a server that adds
      // another date-shaped cause would caption it «нет курса»: #66's defect,
      // reappearing through the path built to prevent it.
      return unnameableGap(gap, t("positions.notConverted"));
  }
}

// What the VALUATION cell says, which is the one cell that can have a cause of
// its own (Position.market_value_gap) — the nearer one, and the one that wins
// there. It answers for that cell in both of its states, and the two are not
// the same question, which is why `fallback` is a parameter rather than a
// constant here:
//
//   - the cell HAS a figure that could not be converted. The row's own
//     sentence is the fallback: when the valuation itself converted fine and
//     the row's term is what withheld the base-currency block, that sentence is
//     the true one and already says so. Repeating the valuation's own sentence
//     there would blame a third currency that is not in play; saying nothing
//     would leave an unexplained marker on a figure the reader can see is not
//     in rubles.
//   - the cell has NO figure and renders a dash. The fallback is then the
//     general «оценки нет, причина не названа», because the row's sentence is
//     about the base currency and this dash is not: a valuation this program
//     never struck is missing in every currency.
//
// The three no-figure causes are the whole of #78. Until it, this switch had
// one case and the dash was captioned «Нет котировки» from a literal in the
// markup — an inference from an absent figure, and false on two of the three
// rows it landed on: a crypto or metal position and a bond with no face value
// recorded can both carry a perfectly good quote. Their sentences must
// therefore not send the reader after one, and `type_not_priced`'s must not
// promise a figure at all: no decision to write such a valuation has been
// taken, so «пока» would be an invention.
//
// An unnameable value degrades to the caller's fallback for the same reason
// (#105): what is true of the cell is still true, the cause merely is not
// known, and a build one release behind a server must not answer a cause it
// cannot read with one it can.
function valuationGapTitle(
  t: (key: string) => string,
  gap: MarketValueGap | null,
  fallback: string,
): string {
  switch (gap) {
    case "no_quote":
      return t("positions.noQuote");
    case "type_not_priced":
      return t("positions.notPricedForType");
    case "no_face_value":
      return t("positions.noFaceValue");
    case "no_rate_valuation_currency":
      return t("positions.notConvertedValuationCurrency");
    case null:
      return fallback;
    default:
      return unnameableGap(gap, fallback);
  }
}

// Renders the price shown under the market value amount. The quote date used
// to be shown inline too ("274,49 · 28.07.2026") but that's visual noise for
// a detail nobody reads at a glance — it now lives in the row's `title`
// tooltip instead (see the caller). When the market value was converted from
// a different currency (e.g. a bond's face-value currency), the tooltip also
// names the original, unconverted amount — again tooltip-only, not shown as
// text, per the same "less visual noise" preference. Returns null unless
// both the price and the quote date are present and well-formed — a
// half-rendered hint would be more misleading than no hint at all.
//
// WHAT THE DATE IS, and why the tooltip spends two sentences on it. price_on
// is the trading session the SOURCE attaches this price to — never the day
// this program fetched it (see TickerQuote.On in
// internal/marketdata/provider.go; #90 was the fetch day being stored as the
// price's own, so on a Monday Friday's price was captioned with Monday). With
// the date now true, the date alone still is not: a bare «Цена на 31.07.2026»
// read on a Monday says both "this is the market's last word" and "the quotes
// job stopped a week ago", and nothing on this screen tells the two apart. So
// the caption states what the date is instead of leaving the reader to infer
// freshness the server never claimed.
//
// The wording is constrained by measurements against live ISS, all of them
// recorded in the QuotesFor doc block in internal/marketdata/moex/moex.go, and
// every constraint is a sentence this caption is forbidden to write:
//   - it is NOT the session's closing price. MOEX publishes that separately as
//     PREVLEGALCLOSEPRICE and the two are different numbers on the same row.
//   - it is NOT necessarily a price anything traded at that day. 779 of TQCB's
//     3021 rows in one measured session carried a price for a paper that did
//     not trade at all, so «сделки в тот день могло и не быть» is the fact,
//     and it is stated as a possibility because that is what was proven.
//   - it is NOT "the previous session" as far as THIS component can know. The
//     wire carries whatever day the source named; "previous" is a property of
//     MOEX's PREVPRICE, not of the contract this screen reads, and a caption
//     that asserted it would be a claim about a provider the frontend has
//     never heard of.
//   - it does NOT say the data is fresh. A date in the past is also exactly
//     what a broken refresh looks like, and reassurance here would cover for
//     one.
//
// The second sentence is the project's "three rates for three questions" rule
// made legible on this cell: the valuation IS struck from this price (see
// marketValue in internal/portfolio/http.go — every path that publishes
// Position.price publishes a valuation computed from it, and this hint is only
// rendered when there is one), while any conversion of that valuation is done
// at the current rate, never at the quote's date — toAPI converts into the
// position's currency and positionInBase into the base one, both at the
// request's `now`. Its conditional shape is load-bearing rather than timid: a
// position whose valuation is already in its own currency, viewed in native
// mode, converts nothing at all, and an unconditional «пересчитана по
// текущему курсу» would name a conversion that never happened. The rate's own
// DATE is deliberately not repeated here — MoneyCell prints it on the amount
// itself («Пересчитано по текущему курсу (на 20.07.2026)»), and two places
// stating one date is two places to drift apart.
//
// WHAT THE NUMBER IS depends on the instrument, and that is the whole of #32
// and #76 — one defect in two halves, a quote printed with nothing saying what
// its unit is.
//
// For a share or an ETF the quote is MONEY PER UNIT, in the currency the QUOTE
// is denominated in. That is normally the position's own, and nothing enforces
// it: Position.currency comes from the operation, a quote carries a currency
// column of its own, and where the two differ the server converts the
// VALUATION into the position's currency and discloses the original in
// market_value_source_currency — while Position.price stays exactly as quoted
// and is never converted. So the price's currency is that source field when it
// is set and market_value_currency otherwise, which is what the contract says
// in as many words (Position.price). BOTH readings are needed: on the ordinary
// row the two agree and either would do, and on the row where a quote is
// denominated in something other than the position's currency they do not, and
// market_value_currency then describes the CONVERTED figure above rather than
// this one.
//
// Naming it unconditionally, rather than only where the reader might be
// confused, is a decision (#76). The bare number was wrong in base mode — the
// valuation converts with the toggle and this line does not, so «274 950,00 ₽»
// stood over «305,50», which was dollars — and a mode-aware sign would be a
// caption whose truth depended on which toggle the reader last touched, silently
// wrong the moment he flipped it. The suffix has to exist for the mismatched-
// quote row anyway; having it appear and disappear as well would be two
// renderings of one number, one of them correct only in context.
//
// For a BOND the quote is a PERCENTAGE OF FACE VALUE (the MOEX convention):
// the server publishes q.Price untouched in Position.price, and marketValue()
// in internal/portfolio/http.go multiplies it as faceValueMinor × price/100 ×
// quantity. The demo seed's ОФЗ 26238 makes the gap concrete — face value
// 1 000,00 ₽, quote 95.20, so one bond is worth 952 ₽ while the line under its
// ruble valuation reads "95,20". Bare, that is a money figure ten times too
// small, sitting under a money figure. It gets «%» and NO currency sign: a
// percentage is denominated in nothing, and both of the fields a share reads
// its currency from are filled in on a bond's row with the FACE value's
// currency and the position's, neither of which this number is in.
//
// The alternative fix — deriving the 952 ₽ and showing THAT — was rejected on
// two grounds. It is money arithmetic in the browser (face value × percent,
// with a rounding decision of its own), which this project does everywhere on
// the server; there is no per-unit price on the wire to render instead. And
// even done correctly it would replace the number the exchange actually
// quotes, the one the owner sees in his broker's app, with one no venue
// prints. So the quote stays as quoted, and says which unit it is in. Reading
// a currency CODE off the payload is not arithmetic and does not touch that
// rule: no figure is computed here, only labelled.
//
// Any other type keeps a bare number. Today that branch cannot be reached from
// this server — it prices share, etf and bond and nothing else, so a crypto or
// metal row carries no valuation, no price, and no hint at all (marketValue's
// `default`, published as market_value_gap: type_not_priced). It is written
// out rather than folded into the share/etf case because the day a newer
// server prices a new type, what its Position.price means will be decided
// there and not here, and a client one release behind must not answer that
// question with the valuation's currency. Same rule as the gap captions
// (#105): what is known stays said, what is not stays unsaid.
function priceHint(
  t: (key: string, opts?: Record<string, string>) => string,
  position: Position,
): { price: string; title: string } | null {
  if (!position.price || !position.price_on) return null;
  const formatted = formatPrice(position.price);
  // price_on, and nothing else on the position: it is the only field that
  // dates the PRICE. in_base.rate_on is a plausible-looking neighbour that
  // dates the valuation's fx conversion instead, and putting it here would
  // print a real date under a sentence about the wrong thing.
  const date = formatDate(position.price_on);
  if (formatted === null || !date) return null;
  let price = formatted;
  let title =
    t("positions.priceOn", { date }) + "\n" + t("positions.priceSession");
  // A switch over the instrument's type rather than "bond or else", so the
  // claim each branch makes is made only where it has been checked. Written
  // with literal keys at every t() call site — the only shape
  // scripts/check-i18n.mjs can verify.
  switch (position.instrument.type) {
    case "bond": {
      // MONEY FIRST, THE PERCENT BESIDE IT. A bond is quoted as a percentage
      // of par, which is what the market trades and what the broker's own app
      // shows — so the percent stays. But every other figure in the row is
      // money, and a percent cannot be compared with a basis; the reader was
      // left multiplying by the face value in their head.
      //
      // The face value is a SNAPSHOT taken when the paper was catalogued and
      // is not refreshed, so on an amortizing bond it drifts. That is not a
      // new risk introduced here: the market valuation in this very row is
      // already the same face value times the same percent (see
      // portfolio.marketValue). This makes an input that was always in use
      // visible, rather than adding one.
      //
      // THE MONEY COMES FROM THE SERVER (price_money_minor), which is where
      // face x price/100 is multiplied and rounded — once, on the figure that
      // is published. Multiplying it here would be money arithmetic on the
      // client, and there is exactly one exemption from that rule in this
      // program (the profit percentage, which is not money).
      const perUnitMinor = position.price_money_minor;
      const faceCurrency = position.instrument.face_currency;
      const perUnit =
        perUnitMinor != null && faceCurrency
          ? formatMinor(perUnitMinor, faceCurrency)
          : null;
      price = perUnit
        ? t("positions.priceMoneyAndPercent", {
            money: perUnit,
            price: formatted,
          })
        : t("positions.pricePercent", { price: formatted });
      title += "\n" + t("positions.priceIsPercentOfFace");
      if (perUnit) title += "\n" + t("positions.priceMoneyFromFace");
      break;
    }
    case "share":
    case "etf": {
      const quoteCurrency =
        position.market_value_source_currency ?? position.market_value_currency;
      // The `?? formatted` is unreachable rather than defensive: this hint is
      // only built for a row that HAS a valuation, which is a row that has a
      // market_value_currency, and formatPrice already answered on this very
      // string. It falls back to the bare number all the same, because the
      // number is the position's own datum and dropping the line would hide
      // it to protect a sign.
      if (quoteCurrency)
        price = formatPriceIn(position.price, quoteCurrency) ?? formatted;
      break;
    }
  }
  title += "\n" + t("positions.priceValuationRate");
  const sourceCurrency = position.market_value_source_currency;
  const sourceMinor = position.market_value_source_minor;
  if (sourceCurrency != null && sourceMinor != null) {
    title +=
      "\n" +
      t("positions.convertedFrom", {
        amount: formatMinor(sourceMinor, sourceCurrency),
      });
  }
  return { price, title };
}

// The income this position received in currencies OTHER than its own, each
// figure formatted in the currency it actually arrived in and joined with the
// same "·" the realized total uses for the same purpose. Returns null when
// there is none, which is the ordinary row.
//
// WHY THE COLUMN NEEDS A SECOND LINE AT ALL. Position.income_minor is only the
// entry of income_by_currency denominated in the position's own currency (the
// contract says so in as many words), and a Russian broker routinely pays a
// yuan bond's coupon and a dollar share's dividend in rubles. On such a row
// that field is 0 — true to the kopeck, and on its own indistinguishable from a
// paper that has never paid anything. The whole point of this line is that the
// money paid in the other currency is DRAWN rather than left out of a figure
// that then reads as "nothing was paid": it is on the row, not behind a hover.
//
// NOTHING IS ADDED HERE, and this is where it would be tempting: two currencies
// summed into one number are denominated in nothing, and converting them needs
// rates the browser does not have and this project does not do in the browser
// anyway. So the entries stay side by side, each under its own sign — the
// server's order, which is by currency code and is the same for two accounts
// holding the same payments in a different journal order.
//
// The position's own currency is filtered out because the figure above already
// carries it. Comparing the two codes is not inferring anything the server
// withheld: `income_minor` IS defined as the entry for `currency`, so the
// filter removes exactly what is already on screen and nothing else.
function otherCurrencyIncome(position: Position): string | null {
  const others = position.income_by_currency.filter(
    (entry) => entry.currency !== position.currency,
  );
  if (others.length === 0) return null;
  return others
    .map((entry) => formatMinor(entry.income_minor, entry.currency))
    .join(" · ");
}

// Formats the unrealized P&L as a percentage of cost ("+12,3 %" / "-12,3 %").
// This is a *display* ratio, not a money amount, so it's computed with plain
// number arithmetic here rather than routed through money.ts (per project
// convention, money.ts owns minor-unit amounts, not derived percentages).
// Returns null when cost is 0 — there is no honest percentage to show for a
// division by zero, so the caller omits the line entirely rather than
// display "Infinity %" or similarly nonsensical output.
function unrealizedPercent(
  unrealizedMinor: number,
  costMinor: number,
): string | null {
  if (costMinor === 0) return null;
  const ratio = unrealizedMinor / costMinor;
  return new Intl.NumberFormat("ru-RU", {
    style: "percent",
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
    signDisplay: "exceptZero",
  }).format(ratio);
}

export function PositionsTable({
  positions,
  cash,
  mode,
  baseCurrency,
}: {
  positions: Position[];
  // THE MONEY, AMONG THE PAPERS. Cash is a holding: yuan on the account was
  // bought at one rate and is worth another today, and that difference is money
  // made or lost exactly as a share's is. It arrives from the server already
  // valued, one row per currency the account has ever touched.
  //
  // Optional because the rows are new and not every caller has them yet; absent
  // and empty render the same — no money rows at all.
  cash?: CashPosition[];
  mode: DisplayCurrencyMode;
  // The space's base currency (Summary.base_currency) — needed to tell
  // "already in base, nothing to convert" apart from "conversion failed, no
  // fx rate" when a position's in_base is null (see resolveDisplayAmount).
  baseCurrency: string;
}) {
  const { t } = useTranslation();
  // A position row's money amounts are denominated in the position's own
  // currency, the quote's, or a bond face value's — never "the account's",
  // which is what the default MoneyCell wording says, so every cell below is
  // handed wording of its own.
  //
  // WHICH wording is not this screen's to work out. Five different things
  // leave a figure unconverted — four that stop the row's whole base-currency
  // block and one that stops its valuation alone — and they are five different
  // pieces of news to the person reading the row, one of them about a figure
  // that is never coming. Only the server knows which of them actually
  // happened, and it says so in Position.in_base_gap and
  // Position.market_value_gap; rowGapTitle and valuationGapTitle above turn
  // those answers into sentences and add nothing. Nothing here infers a cause
  // from a flag or from comparing two currency codes: an inferred cause is a
  // second answer waiting to disagree with the server's, and «нет курса» over
  // a row whose problem is a missing DATE is precisely the disagreement this
  // screen shipped with (#66).
  //
  // Both are per-row, hence resolved inside the map below.
  //
  // Only the market value is converted at today's rate, so only it keeps
  // MoneyCell's default "converted at the current rate (on <date>)" wording.
  // The ruble basis is built lot by lot at each purchase date's rate and
  // income operation by operation at each operation date's rate, so those
  // cells say which historical rates stand behind them instead.
  //
  // The date argument MoneyCell offers is deliberately ignored here:
  // in_base.rate_on is the rate date of the market VALUATION (that is what
  // the API contract says it is) and describes none of these three figures —
  // printing it under them would name a date that had no part in the number
  // above it.
  const costConvertedTitle = () => t("positions.convertedAtPurchaseRates");
  // EACH CELL NAMES THE RATES BEHIND ITS OWN NUMBER, and the three answers
  // differ because the figures are made of different things. The settled figure
  // is realized result plus income: every term is a past event with a date of
  // its own, so every rate is that date's. The total adds the unrealized half,
  // which is today's valuation at today's rate — one sentence over both would
  // be false about one of them. The income line under the settled cell keeps
  // the payment-rate wording, which is all it is.
  const settledConvertedTitle = () => t("positions.convertedSettled");
  const totalConvertedTitle = () => t("positions.convertedTotal");
  const profitConvertedTitle = () => t("positions.convertedProfitMixed");

  // CLOSED ROWS ARE HIDDEN BY DEFAULT AND NEVER IN SILENCE. A position sold out
  // of keeps a realized result and income that are real history, so it is not
  // noise — but it is over, and a portfolio of a few holdings should not open
  // with a screen of them. The count beside the control is what makes hiding
  // honest: the reader is told a number is missing from the list rather than
  // discovering it.
  //
  // THE TOTALS ARE NOT FILTERED WITH THE ROWS, and the control says so. They
  // are the account's, computed on the server over every position, and a header
  // reading «Реализовано 50 000 ₽» over rows that show none of it would look
  // like an error rather than like the deliberate answer it is.
  const [showClosed, setShowClosed] = useState(false);
  const closedCount = positions.filter((p) => p.quantity === "0").length;
  const shown = showClosed
    ? positions
    : positions.filter((p) => p.quantity !== "0");
  // A currency the account has touched and holds nothing of is hidden by the
  // same control, for the same reason a paper it has sold out of is — with one
  // difference: an empty balance is not «closed», so it is not counted in the
  // control's number. It reappears with the closed rows, since that is the
  // control a reader reaches for when looking for what is no longer held.
  const shownCash = (cash ?? []).filter(
    (c) => showClosed || c.amount_minor !== 0,
  );

  return (
    <>
      {closedCount > 0 && (
        <div className="mb-2 flex flex-wrap items-center gap-2 text-sm">
          <Button
            variant="outline"
            size="sm"
            data-testid="toggle-closed-positions"
            onClick={() => setShowClosed((v) => !v)}
          >
            {showClosed ? t("positions.hideClosed") : t("positions.showClosed")}
          </Button>
          <span
            className="text-muted-foreground"
            data-testid="closed-positions-note"
          >
            {showClosed
              ? t("positions.closedShown", { count: closedCount })
              : t("positions.closedHidden", { count: closedCount })}
          </span>
        </div>
      )}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t("positions.columns.instrument")}</TableHead>
            <TableHead className="text-right">
              {t("positions.columns.quantity")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.cost")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.market")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.profit")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.settled")}
            </TableHead>
            <TableHead className="text-right">
              {t("positions.columns.total")}
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {shown.map((position) => {
            const closed = position.quantity === "0";
            const unconvertedTitle = rowGapTitle(t, position.in_base_gap);
            // Market value's currency can differ from the position's own
            // currency (a bond's face-value currency, for instance), so it is
            // always formatted with market_value_currency, never `currency`.
            const marketValueMinor = position.market_value_minor;
            const marketValueCurrency = position.market_value_currency;
            const hasMarketValue =
              marketValueMinor != null && marketValueCurrency != null;
            // The valuation's own reason, which outranks the row's on its own
            // cell and nowhere else. The two can be set at once and both be
            // true — a row stopped by a dateless lot whose valuation is also
            // stuck in a third currency — and this cell takes the nearer of
            // them, while the rest of the row keeps the one that is true of it.
            //
            // WHICH FALLBACK depends on whether there is a figure in the cell,
            // and the two cases are answered by the same lookup so that one
            // vocabulary of causes reaches both. With a figure, an unnamed cause
            // means the row's own sentence is the true one. With none, the row's
            // sentence would be false — it is about the base currency, and a
            // valuation that was never struck is missing in every currency — so
            // the general "no valuation, cause not named" phrase stands instead.
            const valuationUnconvertedTitle = valuationGapTitle(
              t,
              position.market_value_gap,
              hasMarketValue ? unconvertedTitle : t("positions.noValuation"),
            );
            const hint = hasMarketValue ? priceHint(t, position) : null;
            const unrealizedMinor = position.unrealized_pnl_minor;
            const hasUnrealized = unrealizedMinor != null;
            // Cost is resolved unconditionally: it's always present (unlike
            // market value / unrealized P&L), and the profit percentage below
            // needs it alongside the resolved profit figure. Both numbers in
            // that ratio must live in the same currency, which the percentage
            // asserts for itself rather than assuming: in_base carries a null
            // unrealized_pnl_minor whenever the valuation could not be
            // expressed in the position's currency, so cost and profit do not
            // always resolve the same way.
            // One term of the row's converted block, welded to the currency that
            // block says its figures are in (PositionInBase.currency, required
            // by the contract) — never to the session's base currency, which is
            // a second answer to the same question and comes apart from this one
            // whenever a cached row outlives a change of base currency (#106).
            // The term is picked by a function of the block rather than passed
            // in, so a caller cannot hand over the position's OWN figure by
            // mistake and have it printed under the base currency's sign.
            const inBase = position.in_base;
            const convertedTerm = (
              term: (
                block: NonNullable<typeof inBase>,
              ) => number | null | undefined,
            ) =>
              inBase && {
                amountMinor: term(inBase),
                currency: inBase.currency,
                rateOn: inBase.rate_on,
              };
            const resolvedCost = resolveDisplayAmount(
              mode,
              position.currency,
              position.cost_minor,
              baseCurrency,
              convertedTerm((block) => block.cost_minor),
            );
            const resolvedMarketValue = hasMarketValue
              ? resolveDisplayAmount(
                  mode,
                  marketValueCurrency,
                  marketValueMinor,
                  baseCurrency,
                  convertedTerm((block) => block.market_value_minor),
                )
              : null;
            const resolvedUnrealized = hasUnrealized
              ? resolveDisplayAmount(
                  mode,
                  position.currency,
                  unrealizedMinor,
                  baseCurrency,
                  convertedTerm((block) => block.unrealized_pnl_minor),
                )
              : null;
            const resolvedIncome = resolveDisplayAmount(
              mode,
              position.currency,
              position.income_minor,
              baseCurrency,
              convertedTerm((block) => block.income_minor),
            );
            // The second line under the income, and WHETHER it is drawn is
            // decided by what the cell above ended up showing rather than by
            // which mode the toggle is in — the two are not the same question,
            // and using the mode would be wrong in both directions.
            //
            // The converted figure (in_base.income_minor) is the whole income
            // already, every payment brought out of the currency it arrived in,
            // so listing those payments a second time beneath it would show the
            // same money twice and invite the reader to add it to a sum that
            // already contains it.
            //
            // Everything else shows the position's OWN figure, which carries one
            // currency and cannot carry the rest — and that is three situations,
            // not one: the toggle asks for the position's currency; the toggle
            // asks for the base currency and the row's block could not be struck
            // (`noRate`, captioned by the row's own sentence); or the position's
            // currency IS the base currency, so the server publishes no block at
            // all and never had to. That last one is the case this line exists
            // for above all: a ruble paper paid a dollar dividend has no
            // conversion object in EITHER mode, so before this its row showed a
            // ruble zero in both, with nothing on it saying a dividend had been
            // paid at all. (The journal below still listed the payment itself —
            // this is about the position's own row, which is where a reader asks
            // what the paper has earned.)
            const otherIncome = resolvedIncome.converted
              ? null
              : otherCurrencyIncome(position);
            // The three figures that can be missing in ONE currency and present
            // in the other, which is why they go through the optional resolver:
            // a disposal settled in a third currency leaves no realized figure
            // (and so no settled, and so no total) in the position's own
            // currency, while the converted block has all three.
            const resolvedRealized = resolveOptionalDisplayAmount(
              mode,
              position.currency,
              position.realized_pnl_minor,
              baseCurrency,
              convertedTerm((block) => block.realized_pnl_minor),
            );
            const resolvedSettled = resolveOptionalDisplayAmount(
              mode,
              position.currency,
              position.settled_minor,
              baseCurrency,
              convertedTerm((block) => block.settled_minor),
            );
            const resolvedTotal = resolveOptionalDisplayAmount(
              mode,
              position.currency,
              position.total_minor,
              baseCurrency,
              convertedTerm((block) => block.total_minor),
            );
            // What the settled figure is made of, spelled out where the column
            // that used to show one of its halves used to be. The income column
            // is gone and its number lives on here: a reader who wants to know
            // why «Зафиксировано» is what it is gets the two terms rather than
            // being told to trust the sum.
            const settledHint = resolvedSettled
              ? t("positions.settledHint", {
                  realized: resolvedRealized
                    ? formatMinor(
                        resolvedRealized.amountMinor,
                        resolvedRealized.currency,
                      )
                    : "—",
                  income: formatMinor(
                    resolvedIncome.amountMinor,
                    resolvedIncome.currency,
                  ),
                })
              : // WHICH OF THE TWO REASONS IT IS, ASKED OF THE DATA RATHER THAN
                // GUESSED. The server withholds this figure on two unrelated
                // grounds — a disposal settled in a currency the position is not
                // denominated in, or payments that arrived in more than one
                // currency — and a caption naming the first over a paper nobody
                // ever sold would be a false reason beside a true dash, which is
                // the failure this codebase has been caught at four times.
                //
                // realized_pnl_minor is null on exactly the first ground (see
                // Position.realized_pnl_minor in the contract), so the two are
                // told apart from published fields with nothing inferred. The
                // dash itself is reached only when the POSITION-currency figure
                // is missing — a figure present in the base currency is shown
                // rather than dashed — so the reason is about the position's own
                // currency in either mode.
                position.realized_pnl_minor == null
                ? t("positions.settledMissingSale")
                : t("positions.settledMissingIncome");
            const unrealizedPct =
              resolvedUnrealized &&
              resolvedUnrealized.currency === resolvedCost.currency
                ? unrealizedPercent(
                    resolvedUnrealized.amountMinor,
                    resolvedCost.amountMinor,
                  )
                : null;
            // A bare "+10,0 %" means a different thing per mode: in the
            // position's own currency it is the instrument's move alone, in the
            // base currency it carries the fx move too, so one position can
            // honestly read +10 % in one mode and -45 % in the other. Naming
            // the currency is what makes those two answers legible as answers
            // to different questions rather than as a discrepancy.
            //
            // The currency named is the one the ratio was actually computed in
            // (resolvedCost's — the guard above already proved the profit's
            // equal to it), not the one the mode asked for: in base mode with
            // no converted figure available both figures stay native, and the
            // label has to follow the numbers.
            const unrealizedPctTitle = t("positions.profitPercentIn", {
              currency: resolvedCost.currency,
            });
            // Why the profit cell is empty, as ONE string used twice — the
            // tooltip's and the screen reader's copy are the same value, so
            // there is no arrangement in which they say different things. The
            // line break survives in the tooltip and collapses to a space in the
            // markup, which is the right rendering in each place and needs no
            // second version to get it.
            //
            // Two ways to have no profit, and they are two different sentences.
            // With a valuation present, the profit is missing because that
            // valuation is in another currency and cannot be subtracted from the
            // basis. With none, the profit is missing because one of its two
            // operands is — so this cell says that in its own words and then
            // hands over to the valuation's cause, whatever the server said it
            // was. It used to print «Нет котировки» flat, which is the same false
            // sentence #78 is about, one column over: a crypto row's profit is
            // not waiting for a quote either.
            const profitDashHint = hasMarketValue
              ? t("positions.currencyMismatch")
              : t("positions.profitNeedsValuation") +
                "\n" +
                valuationUnconvertedTitle;
            return (
              <TableRow
                key={position.instrument.id}
                className={cn(closed && "opacity-50")}
              >
                <TableCell>
                  <div className="font-medium">
                    {position.instrument.name}
                    {position.instrument.frozen && (
                      <Badge variant="outline" className="ml-2">
                        {t("positions.frozen")}
                      </Badge>
                    )}
                    {closed && (
                      <Badge variant="outline" className="ml-2">
                        {t("positions.closed")}
                      </Badge>
                    )}
                  </div>
                  <div className="text-xs text-muted-foreground">
                    {position.instrument.ticker}
                  </div>
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {position.quantity}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  <MoneyCell
                    resolved={resolvedCost}
                    notConvertedTitle={unconvertedTitle}
                    convertedTitle={costConvertedTitle}
                    testId="position-cost"
                  />
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {hasMarketValue && resolvedMarketValue ? (
                    <>
                      <MoneyCell
                        resolved={resolvedMarketValue}
                        notConvertedTitle={valuationUnconvertedTitle}
                        testId="position-market-value"
                      />
                      {hint && (
                        <div
                          data-testid="position-price"
                          className="text-xs font-normal text-muted-foreground"
                          title={hint.title}
                        >
                          {hint.price}
                        </div>
                      )}
                    </>
                  ) : (
                    <span
                      data-testid="position-no-quote"
                      className="text-muted-foreground"
                      title={valuationUnconvertedTitle}
                    >
                      {/* The dash is a drawing of an empty cell and says nothing
                        on its own, so it is hidden from assistive technology
                        and the sentence beside it is what gets read (#31). The
                        other order — announcing "dash" and then leaving the
                        reason in a `title` no screen reader is obliged to
                        surface on a non-focusable span — is how this cell told
                        a sighted reader why the number is missing and told
                        everyone else nothing. */}
                      <span aria-hidden="true">—</span>
                      <span className="sr-only">
                        {valuationUnconvertedTitle}
                      </span>
                    </span>
                  )}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {resolvedUnrealized ? (
                    <>
                      <MoneyCell
                        resolved={resolvedUnrealized}
                        className={signClass(resolvedUnrealized.amountMinor)}
                        notConvertedTitle={unconvertedTitle}
                        convertedTitle={profitConvertedTitle}
                        testId="position-profit-amount"
                      />
                      {unrealizedPct && (
                        <div
                          data-testid="position-profit-percent"
                          className="text-xs font-normal text-muted-foreground"
                          title={unrealizedPctTitle}
                        >
                          {unrealizedPct}
                        </div>
                      )}
                    </>
                  ) : (
                    <span
                      data-testid="position-profit-dash"
                      className="text-muted-foreground"
                      title={profitDashHint}
                    >
                      <span aria-hidden="true">—</span>
                      <span className="sr-only">{profitDashHint}</span>
                    </span>
                  )}
                  {/* THE REALIZED RESULT, UNDER THE UNREALIZED ONE AND NOT
                    INSTEAD OF IT. The column says «Прибыль» and means the
                    valuation less the basis; a closed position's is honestly
                    nothing, and before this line such a row showed 0,00 ₽
                    under that heading for a paper that had made 1 940,42 ₽.
                    Folding the two into one cell would put two different
                    figures under one word, which is how a true number ends up
                    under a false caption — the failure this codebase has been
                    caught at repeatedly.

                    Drawn only when there IS a realized result, so a position
                    nobody has ever sold out of does not carry a row of
                    «реализовано 0,00» it has no use for. */}
                  {resolvedRealized && resolvedRealized.amountMinor !== 0 && (
                    <div
                      data-testid="position-realized"
                      className="text-xs font-normal text-muted-foreground"
                      title={t("positions.realizedHintRow")}
                    >
                      {t("positions.realizedOnRow", {
                        amount: formatMinor(
                          resolvedRealized.amountMinor,
                          resolvedRealized.currency,
                        ),
                      })}
                    </div>
                  )}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  title={settledHint}
                >
                  {resolvedSettled ? (
                    <MoneyCell
                      resolved={resolvedSettled}
                      notConvertedTitle={unconvertedTitle}
                      convertedTitle={settledConvertedTitle}
                      testId="position-settled"
                    />
                  ) : (
                    <span
                      data-testid="position-settled-dash"
                      className="text-muted-foreground"
                    >
                      <span aria-hidden="true">—</span>
                      <span className="sr-only">{settledHint}</span>
                    </span>
                  )}
                  {otherIncome && (
                    <div
                      data-testid="position-income-other-currency"
                      className="text-xs font-normal text-muted-foreground"
                      title={t("positions.incomeOtherCurrencyHint")}
                    >
                      {t("positions.incomeOtherCurrency", {
                        amounts: otherIncome,
                      })}
                    </div>
                  )}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  title={t("positions.totalHint")}
                >
                  {resolvedTotal ? (
                    <MoneyCell
                      resolved={resolvedTotal}
                      notConvertedTitle={unconvertedTitle}
                      convertedTitle={totalConvertedTitle}
                      testId="position-total"
                    />
                  ) : (
                    <span
                      data-testid="position-total-dash"
                      className="text-muted-foreground"
                    >
                      <span aria-hidden="true">—</span>
                      <span className="sr-only">
                        {t("positions.totalMissing")}
                      </span>
                    </span>
                  )}
                </TableCell>
              </TableRow>
            );
          })}
          {shownCash.map((money) => {
            // WHAT THE ROW SHOWS DEPENDS ON THE MODE, and the two are not the
            // same question. In the account's own currencies the money is
            // simply itself — a thousand yuan, costing a thousand yuan — so the
            // row prints the balance and leaves the money columns empty rather
            // than filling three of them with one figure. In the base currency
            // it has a cost, a value and a profit like any other holding, and
            // that is the whole reason it is on this screen.
            const inBase = money.in_base;
            const showInBase =
              mode === "base" && money.currency !== baseCurrency;
            return (
              <TableRow key={`cash-${money.currency}`} data-testid="cash-row">
                <TableCell>
                  <div className="font-medium" data-testid="cash-currency">
                    {t("positions.cashName", { currency: money.currency })}
                  </div>
                  <div className="text-xs text-muted-foreground">
                    {t("positions.cashKind")}
                  </div>
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  data-testid="cash-amount"
                >
                  {formatMinor(money.amount_minor, money.currency)}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  data-testid="cash-cost"
                >
                  {showInBase && inBase.cost_minor != null
                    ? formatMinor(inBase.cost_minor, inBase.currency)
                    : ""}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  data-testid="cash-value"
                >
                  {showInBase && inBase.value_minor != null
                    ? formatMinor(inBase.value_minor, inBase.currency)
                    : ""}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  title={showInBase ? t("positions.cashProfitHint") : undefined}
                >
                  {showInBase && inBase.unrealized_pnl_minor != null ? (
                    <span
                      data-testid="cash-profit"
                      className={signClass(inBase.unrealized_pnl_minor)}
                    >
                      {formatMinor(
                        inBase.unrealized_pnl_minor,
                        inBase.currency,
                      )}
                    </span>
                  ) : (
                    ""
                  )}
                </TableCell>
                {/* Settled and total belong to papers: money that was never
                    sold has locked in nothing and paid nothing. Left empty
                    rather than written as noughts, which would read as figures
                    a reader could add up. */}
                <TableCell />
                <TableCell />
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </>
  );
}
