import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Trash2 } from "lucide-react";
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
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { formatPriceIn, signClass } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { resolveDisplayAmount } from "@/lib/display-amount";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import { useReportScreenCurrencies } from "@/lib/screen-currencies";
import { MoneyCell } from "@/components/money-cell";
import { costBasisCaveat } from "@/components/cost-basis-notice";
import { unnameableGap } from "@/lib/unnameable-gap";
import {
  useOperations,
  useDeleteOperation,
  isConflict,
  JOURNAL_PAGE_SIZE,
  type Operation,
  type OperationInBaseGap,
} from "@/api/operations";
import { useInstrumentIndex, type Instrument } from "@/api/instruments";
import type { CostBasisRules } from "@/api/tax-residencies";

// Whether this row's amount is not money that moved on the day it is dated but
// a cost basis some rule picked out of earlier purchases — the only kind of
// figure the cost basis caveat is true of.
//
// The answer is the server's, taken from what it publishes ABOUT THIS ROW, and
// deliberately not from a list of operation types kept here. Such a list is a
// copy of a server rule with nothing tying it to the contract: the day the
// server derives a cost basis for one more type, the copy stops matching and
// the caveat disappears from the figure without a word — which is the exact
// bug this whole caveat exists to prevent, reintroduced by the code that
// renders it.
//
// ONE published field answers: assembled_from_lots, a property of the
// OPERATION itself and present whether or not in_base is (see the API
// contract). It says the amount was built piece by piece out of the purchases
// behind it, each at its own day's rate — and the pieces are the ones the
// source account's queue released, so "a rule picked this out of earlier
// purchases" is precisely what it reports. The server sets it from the presence
// of a stored breakdown, not from the row's type, so a new type that carries
// one is covered the day it appears.
//
// has_undated_lots USED TO BE THE OTHER HALF OF THIS, and #81 is why it no
// longer is. That flag covers two different parcels at once: a breakdown with a
// dateless piece among dated ones (assembled_from_lots is true as well there,
// so nothing is lost by dropping it) and a transfer with NO breakdown — a basis
// someone typed in by hand at POST /operations/transfer, or one recorded before
// breakdowns were kept. On that second kind the caveat's own opening sentence
// («её выбрало то же правило очереди, что и стоимость позиций») is false: no
// release happened, no lots stand behind the number, nobody's rule chose it.
// The country notices that follow it are then a paragraph about a computation
// this figure never went through — a warning hung over a row it is not about,
// which is the same defect the caveat was moved off the table header to end.
//
// A LEGACY TRANSFER'S BASIS WAS CHOSEN BY THE QUEUE, and it loses the caveat
// here too. That is deliberate rather than overlooked: the wire cannot tell it
// from a hand-typed one — neither can the database, which stores an absent
// breakdown identically either way — so a caveat shown on those rows would be
// a claim this program has no way to check. Saying nothing about how a figure
// arose is honest; crediting a rule that may not have run is not.
//
// Dropping the flag from here changes nothing about its other job on this
// screen: a transferred parcel with no breakdown still reports its own dateless
// state through in_base_gap, and still gets the sentence that says so.
//
// Until #67, assembled_from_lots lived INSIDE in_base and vanished the moment
// the conversion block did — including for the most ordinary reason a
// conversion block can be absent, `currency` already equalling the base
// currency. A transferred parcel of RUB shares in a RUB-based space (the
// product owner's own case) has a complete, dated breakdown and nothing to
// convert, so it used to publish nothing about being a cost basis at all: not
// a missing rate, not a missing date, just silence. Moving the field onto the
// operation removed that hole without adding a client-side list of types — and
// it is what lets this predicate stand on that field alone now.
function wasAssembledFromLots(operation: Operation): boolean {
  return operation.assembled_from_lots;
}

// The one sentence that captions BOTH money cells of a row, chosen from the
// term the server says it stopped on (Operation.in_base_gap). It is the row's
// only source: has_undated_lots answers the first of these causes on its own
// and cannot disagree with it — both derive from one server-side predicate —
// but a caption assembled from two sources is a caption that will eventually
// be assembled from two sources that have drifted. With #81 that field stopped
// deciding the cost basis caveat as well (see wasAssembledFromLots), so nothing
// on this screen reads it any more — it remains in the contract as a standing
// fact about an operation, published on the create and transfer responses where
// no gap exists at all, and this client simply has no question left that it is
// the right answer to.
//
// Each sentence explains why the whole row carries no base-currency figures,
// which is what makes it true over the amount cell and the fee cell alike:
// in_base is published as a whole or not at all, so a single unvaluable term
// withholds both figures together.
//
// The permanent cause is told apart from the temporary ones in as many words,
// because that is the difference the reader is actually served by. A missing
// rate is a gap the fx backfill can close, after which the ruble figure
// appears; an unrecorded purchase date resolves never, since nobody wrote it
// down and nothing can recover it, and a caption that promised such a row a
// figure would promise one that is not coming.
//
// What the temporary ones may say about the closing is a condition, not a
// promise (#105). Rates come from one source with a currency list of its own,
// so a pair it publishes no leg of never gets a rate at all — and which pairs
// those are is not on the wire: the contract names no source and lists no
// currencies, so this screen cannot qualify the sentence by pair either. It
// states the consequence instead, and only as a consequence — «Если курс
// появится … операция посчитается сама» — which every request makes true, since
// each recomputes from whatever rates exist at the time.
//
// The wordings are the positions screen's wherever the cause is the same
// («восстановить … уже неоткуда», «Если курс появится при обновлении курсов, …
// посчитается сама»): two screens explaining one condition differently is a
// reader's problem, not a translator's — and both tables are stacked on one
// account page, so the reader meets them in one glance.
//
// Written as a switch over literal keys rather than a lookup table, so every
// key stays a literal at the t() call site — the only shape
// scripts/check-i18n.mjs can verify.
function rowGapTitle(
  t: (key: string) => string,
  gap: OperationInBaseGap | null | undefined,
): string {
  switch (gap) {
    case "undated_lot":
      return t("operations.notConvertedUndatedLot");
    case "no_rate_operation_date":
      return t("operations.notConvertedNoRateOperationDate");
    // The whole of #79: this row's amount is a cost basis assembled from
    // purchases, each valued at the rate of the day IT was made, and the day
    // that has no rate is one of those. The transfer's own date usually has a
    // perfectly good rate — it is simply not a rate that may value shares
    // bought on other days — so «нет курса на дату операции» here does not
    // merely fail to explain the row, it states something false about it. The
    // sentence therefore names the rule rather than asserting that the
    // purchases fell on other days: one of them may well have fallen on the
    // transfer's own day, and this cause would read the same.
    case "no_rate_lot_date":
      return t("operations.notConvertedNoRateLotDate");
    case null:
    case undefined:
      // The contract publishes a cause whenever in_base is null and the
      // currency differs from the base one, so a row with a marker and no
      // cause should not occur — and the field is absent altogether on the
      // create and transfer responses, which this table never renders. It is
      // left neither to crash nor to say nothing: the general phrase says only
      // what the payload itself shows — the base-currency figures were
      // withheld — and it is what an older server's payload deserves.
      return t("operations.notConverted");
    default:
      // The other way to reach that phrase, and the one that decided how it is
      // worded (#105). A server NEWER than this build sends a value outside
      // the union above; the sentence shown then may claim nothing about the
      // cause, because not knowing the cause is the very condition that brings
      // it here. «Нет курса», which is what it used to say, claims exactly
      // that — and would be false of a value shaped like `undated_lot`, which
      // is in TODAY's enum and is about a date nobody recorded rather than
      // about a rate. The positions screen says the same thing in the same
      // words for the same reason; the two are edited together.
      return unnameableGap(gap, t("operations.notConverted"));
  }
}

// WHO OWNS A JOURNAL ROW, answered exactly as the server answers it: the
// service refuses to delete anything whose `source` is not "manual" (see
// Service.Delete), because such a row is a projection of records held
// elsewhere — the broker's own, kept in the import's mirror — and is written
// again the next time the projection is rebuilt. A menu offering to delete it
// would be offering something that cannot happen: the request is refused, and
// even if it were not, the row would come back.
//
// The rule is spelled as the server spells it — "manual" and nothing else —
// rather than as "not tinvest": the column allows a third value ('csv',
// migration 0005) that nothing writes today, and a row carrying it must not
// become deletable here merely because this file forgot it exists.
function isImported(operation: Operation): boolean {
  return operation.source !== "manual";
}

// Who wrote the row, drawn beside its type. Т-Инвестиции by name where the row
// says so, and a plain «загружено извне» for any other non-manual source:
// naming an unknown importer after the only one that exists today would be
// stating where the data came from without having been told. Nothing at all on
// a hand-entered row, which is the ordinary case and needs no label.
function SourceBadge({ operation }: { operation: Operation }) {
  const { t } = useTranslation();
  if (!isImported(operation)) return null;
  const tinvest = operation.source === "tinvest";
  return (
    // THE HINT BRANCHES WHERE THE LABEL DOES, and for the same reason. What is
    // true of every imported row is that this program will not delete it; that
    // it is WRITTEN AGAIN afterwards is true of the T-Invest import alone,
    // which rebuilds its rows from the broker's mirror. A 'csv' row (migration
    // 0005 allows the value, nothing writes it today) has nothing behind it to
    // rebuild from, so promising that one comes back would be inventing a
    // machine that does not exist.
    <Badge
      variant="outline"
      title={tinvest ? t("operations.importedTinvestTitle") : t("operations.importedTitle")}
    >
      {tinvest ? t("connections.tinvest") : t("operations.importedElsewhere")}
    </Badge>
  );
}

// TradingModeBadge says WHERE an operation happened, when anything said it.
//
// IT SHOWS THE BROKER'S CODE AND NEVER REPLACES IT. This program can name only
// some of the codes — the exchange boards it has a published index for, and
// the one whose own name says it is over-the-counter — so the badge carries
// the code itself and adds a word only where a word is sourced. An unnamed
// code says so in its hint instead of being dressed in a guess.
//
// IT IS NOT A TAX STATEMENT. «Обращающаяся» is a property of the SECURITY —
// admitted to trading and quoted — not of where one deal was struck, so the
// hint says this is worth looking at when a tax report is built and stops
// there. Naming an off-exchange purchase «необращающаяся» would be a caption
// this program cannot stand behind.
function TradingModeBadge({ operation }: { operation: Operation }) {
  const { t } = useTranslation();
  const code = operation.trading_mode;
  const kind = operation.trading_mode_kind;
  if (!code) return null;
  const known = kind && kind !== "unknown";
  return (
    <Badge
      variant="outline"
      data-testid="operation-trading-mode"
      title={t(`tradingModeTitles.${known ? kind : "unknown"}`)}
    >
      {known ? `${t(`tradingModes.${kind}`)} · ${code}` : code}
    </Badge>
  );
}

export function OperationsTable({
  accountId,
  canDelete,
  mode,
  baseCurrency,
  costBasisRules,
}: {
  accountId: string;
  // Delete action is editor+ (owner/editor); viewers never see it.
  canDelete: boolean;
  mode: DisplayCurrencyMode;
  // The space's base currency (SessionInfo.base_currency) — needed to tell
  // "already in base, nothing to convert" apart from "no fx rate for that
  // date" when an operation's in_base is null (see resolveDisplayAmount).
  baseCurrency: string;
  // Whether the earliest-purchases-first queue behind a transferred parcel's
  // amount is the queue the owner's country requires
  // (SessionInfo.cost_basis_rules). It arrives as a prop from the screen,
  // which has the session already, rather than being fetched here or shipped
  // a third time inside this listing: the statement belongs to the space, and
  // one truth in three payloads is two more places to forget it (see the API
  // contract). Undefined while the session is still loading — the caveat
  // simply waits rather than guessing.
  costBasisRules?: CostBasisRules;
}) {
  const { t } = useTranslation();
  // "Show more" fetches the next page and appends it (see useOperations). The
  // backend returns a stable occurred_on/created_at DESC order, so consecutive
  // offsets partition the journal with nothing repeated and nothing skipped.
  const operations = useOperations(accountId, JOURNAL_PAGE_SIZE);
  // Instrument names, by id, for the whole catalog — not one page of it.
  //
  // This used to be the picker's own first-page query, and it carried a note
  // saying that an instrument past the fiftieth would fall back to an id suffix
  // and that this was not worth an issue while the catalog stayed small. The
  // catalog is not small any more: one broker import brings in around a hundred
  // papers and every sync adds to them, so from the fiftieth on — roughly half
  // that catalog — the column had nothing to print but «#a1b2c3d4» (#104). A
  // listing may honestly stop where the reader stopped scrolling; a lookup
  // cannot, because nothing on the row says a name exists and simply was not
  // fetched. See useInstrumentIndex for what the walk costs.
  const instruments = useInstrumentIndex();
  const deleteOperation = useDeleteOperation();
  const [deleteTarget, setDeleteTarget] = useState<Operation | null>(null);
  const list = operations.data?.pages.flatMap((page) => page.operations) ?? [];

  // The journal reports its own currencies to the screen-wide counter that
  // decides whether the header's display-currency toggle is shown. It reports
  // separately from the screen component (see detail.tsx) because it owns its
  // own query, and its currencies can be ones nothing else on the screen
  // knows about: a foreign-currency operation on a base-currency account is
  // otherwise invisible to the counter, leaving the user with amounts they
  // cannot switch. Only currencies in the pages loaded so far (`list`) are
  // counted — if the sole foreign-currency operation sits past row 50, the
  // toggle won't appear until "Show more" is clicked. That's consistent with
  // what the table actually shows, so it's accepted rather than worked around.
  // Must run unconditionally, before the early returns below, per the Rules of
  // Hooks.
  useReportScreenCurrencies([
    ...list.map((operation) => operation.currency),
    // The conversion target belongs in the set too, so a journal that is
    // entirely in one *foreign* currency still counts as two.
    ...(baseCurrency ? [baseCurrency] : []),
  ]);

  // A journal row's amounts are in the operation's own currency, which is not
  // necessarily the account's (a foreign-currency operation can sit on a
  // base-currency account), so the default MoneyCell wording would name the
  // wrong thing. The converted-amount wording is journal-specific too: these
  // figures use the rate of the day the operation happened. Account balances
  // and a position's market value use today's; a position's cost and income
  // are historical like these, but describe their own dates (each lot's
  // purchase, each payout), so they carry their own wording rather than
  // sharing the journal's.
  //
  // WHICH sentence a row that carries no base-currency figures gets is not
  // this screen's to work out. Three different terms can stop the conversion
  // — the operation's own day's rate, a purchase day's rate, and a purchase
  // date nobody ever recorded — and they are three different pieces of news,
  // one of them about a figure that is never coming. Only the server knows
  // which of them actually happened, and it says so in Operation.in_base_gap;
  // rowGapTitle above turns that answer into a sentence and adds nothing.
  //
  // The positions screen draws the same line, over five causes of its own —
  // four that stop a row's whole base-currency block and one that stops its
  // valuation alone — and its wordings are reused here wherever the condition
  // is the same one. What is NOT shared is the number of causes: the two
  // screens sum different terms (a position has income and a valuation; an
  // operation has neither), so each names its own, and neither list is a copy
  // of the other to keep in step.
  //
  // Resolved per row, inside the map below.

  // The caveat that a cost basis here was picked by a queue that is not the
  // owner's country's. It hangs on the amount cells whose figure actually IS
  // one, and nowhere else. It used to be a banner over the whole table, which
  // put "computed by a rule that is not your country's" above fifty deposits,
  // purchases and dividends it says nothing about, and — since the positions
  // above render the same banner — printed the identical paragraph twice on
  // one screen. Undefined when the session is still loading or when the
  // country's rule IS what is computed (see costBasisCaveat).
  const costBasisTitle = costBasisRules ? costBasisCaveat(t, costBasisRules) : undefined;

  // The catalog entry behind a row. Undefined for a row with no instrument at
  // all (a deposit, a fee) and while the catalog is still being read, so every
  // caller has to work without it.
  const instrumentOf = (instrumentId: string | null | undefined): Instrument | undefined =>
    instrumentId ? instruments.get(instrumentId) : undefined;

  const instrumentName = (instrumentId: string | null | undefined) => {
    if (!instrumentId) return "—";
    const found = instrumentOf(instrumentId);
    return found ? found.name : `#${instrumentId.slice(-8)}`;
  };

  // WHAT THE PRICE IN THIS COLUMN IS, which #75 is about because «цена» names
  // two different quantities in this application and both of them are right.
  // Here it is money per unit: what one unit actually cost, and the only price
  // an operation ever records — the trade dialog asks a bond's percentage of
  // face as an input and sends the money field (see trade-dialog.tsx). On the
  // positions table a bond's price is the exchange's quote, a PERCENTAGE of
  // face value, and since #32 that table says so on the cell. Both are on ONE
  // screen — the positions of an account and its journal are stacked on the
  // same page (see detail.tsx) — and the demo stand puts both numbers for one
  // bond on it: 100 ОФЗ bought at 950,00 ₽ apiece down here, «95,20 %» under
  // the same position's valuation up there. Nothing told the reader which was
  // which.
  //
  // Three ways to separate them were considered.
  //   - Show the other quantity too, deriving one from the other. Rejected on
  //     the grounds this project rejects it everywhere: it is money arithmetic
  //     in the browser (face value × percent, with a rounding decision of its
  //     own), and the journal has no face value on the wire to do it with.
  //   - Put a currency on the number — «950,00 ₽». Rejected then as noise, on
  //     a premise that does not hold: «the amount and the fee in this very row
  //     already carry the currency». They carry A currency, and it is this
  //     number's for certain only in the account-currency display mode. In the
  //     base-currency one those two convert wherever a rate allows and this
  //     one never does — it is the price as recorded — so a dollar buy printed
  //     «305,50» under «-27 495,00 ₽», and the row's own figures were what
  //     mislabelled it. The bare number was never unlabelled; it was labelled
  //     by its neighbours, and they are not reliably the right label. That is
  //     #114, and the number now says its currency itself — see the price cell
  //     below, which is where the decision lives.
  //   - Name the unit, in the column header and in the cell's own tooltip.
  //     That is this. The header is visible without hovering anything and
  //     settles every row at once — a percentage of face is not «цена за
  //     единицу» — and the tooltip adds the sentence for the instrument where
  //     the confusion actually lives.
  //
  // The bond sentence is an ADDITION to the general one, never a correction of
  // it, which is what lets the tooltip degrade honestly: an instrument the
  // loaded catalog page does not hold leaves the general sentence standing,
  // and that sentence is true of a bond too — the number here is money per
  // bond whether or not this screen has been told it is a bond.
  //
  // Written as two literal-key branches rather than t(cond ? a : b) so both
  // keys stay verifiable by scripts/check-i18n.mjs, which only reads literals.
  const priceTitle = (instrument: Instrument | undefined): string => {
    const title = t("operations.pricePerUnit");
    if (instrument?.type === "bond") {
      return title + "\n" + t("operations.priceNotPercentOfFace");
    }
    return title;
  };

  const confirmDelete = () => {
    if (!deleteTarget) return;
    deleteOperation.mutate(
      { operationId: deleteTarget.id, accountId },
      { onSuccess: () => setDeleteTarget(null) },
    );
  };

  if (operations.isLoading && !operations.data) {
    return <div className="text-muted-foreground">{t("app.loading")}</div>;
  }
  if (operations.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{t("app.error")}</AlertDescription>
      </Alert>
    );
  }

  if (list.length === 0) {
    return (
      <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
        {t("operations.empty")}
      </div>
    );
  }

  // The server's answer, never the page's length. A page shorter than the one
  // asked for means either the end of the journal or a `limit` the server
  // clamped, and those are opposite facts — reading the length picked the wrong
  // one at exactly the ceiling and hid every older entry (#86).
  const canLoadMore = operations.hasNextPage;

  return (
    <div className="grid gap-3">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t("operations.columns.date")}</TableHead>
            <TableHead>{t("operations.columns.type")}</TableHead>
            <TableHead>{t("operations.columns.instrument")}</TableHead>
            <TableHead className="text-right">{t("operations.columns.qty")}</TableHead>
            <TableHead className="text-right">{t("operations.columns.amount")}</TableHead>
            <TableHead className="text-right">{t("operations.columns.fee")}</TableHead>
            {canDelete && <TableHead className="w-10" />}
          </TableRow>
        </TableHeader>
        <TableBody>
          {list.map((operation) => {
            // Amount and fee are converted and rounded independently by the
            // backend — they are two separate figures, not terms of one
            // total — so each is resolved on its own.
            const inBase = operation.in_base;
            // One term of the row's converted block, welded to the currency
            // that block says its figures are in (OperationInBase.currency,
            // required by the contract) — never to the session's base
            // currency, which is a second answer to the same question and
            // comes apart from this one whenever a cached journal outlives a
            // change of base currency (#106). The term is picked by a function
            // of the block rather than passed in, so the fee cell cannot be
            // handed the amount, nor either cell the operation's own figure.
            const convertedTerm = (
              term: (block: NonNullable<typeof inBase>) => number | null | undefined,
            ) =>
              inBase && {
                amountMinor: term(inBase),
                currency: inBase.currency,
                rateOn: inBase.rate_on,
              };
            const resolvedAmount = resolveDisplayAmount(
              mode,
              operation.currency,
              operation.amount_minor,
              baseCurrency,
              convertedTerm((block) => block.amount_minor),
            );
            const resolvedFee = resolveDisplayAmount(
              mode,
              operation.currency,
              operation.fee_minor,
              baseCurrency,
              convertedTerm((block) => block.fee_minor),
            );
            // Three different things a converted amount can be, and the
            // tooltip has to name the right one — it is read as a statement of
            // fact about the number beside it.
            //
            // A transfer between the family's own accounts carries the cost of
            // the shares behind it, so the backend converts it piece by piece
            // at the rate of each piece's own purchase day
            // (operation.assembled_from_lots) and its headline date is the
            // newest PURCHASE, not the transfer. That case must be checked
            // FIRST, because neither of the other two wordings is true of it
            // and a date comparison cannot tell: the headline rate happens to
            // be dated the transfer day whenever the last purchase was made on
            // it, so relying on the dates picks one false sentence or the
            // other. On the demo data it read "there is no rate for the
            // operation's date — converted at the nearest, 15.06.2026" about a
            // figure assembled from two rates, on a day whose rate exists and
            // was deliberately not used.
            //
            // The sentence names the RULE — each piece at its own purchase
            // day's rate — rather than asserting the purchases fell on days
            // OTHER than the transfer's, the way an earlier wording did
            // ("...сделанных в другие дни ... а не по курсу дня перевода").
            // internal/portfolio/engine.go's CheckTransferLots only rejects a
            // piece dated AFTER the transfer, so a same-day buy-then-transfer
            // is legal: buy on day X, move the whole parcel on day X, and
            // every piece of the breakdown is dated X too. The old wording was
            // false of that row on both halves at once — the purchases were
            // NOT on other days, and the transfer day's rate WAS what struck
            // the figure, because the one purchase day and the transfer day
            // are the same day. The rule-naming form is true whichever way the
            // dates land, exactly like operations.notConvertedNoRateLotDate
            // below for the equivalent unconverted case (#79's fix).
            //
            // WHICH DATE EACH SENTENCE NAMES is the other half, and the two
            // published dates are not interchangeable (see OperationInBase in
            // the API contract). `dated_on` is the day a figure is VALUED at —
            // the operation's own day for an ordinary row, the newest purchase
            // in the parcel for a transfer. `rate_on` is the day the rate that
            // answered actually came from, which is `dated_on` itself or the
            // nearest earlier day that has one: the CBR publishes nothing at
            // weekends and holidays, roughly a third of the calendar. So the
            // transfer's sentence, whose subject is a purchase — «самый
            // поздний из них» — names `dated_on`, and naming `rate_on` there
            // printed a day nothing was bought on (#80). The other two
            // sentences are about the rate and name `rate_on`.
            //
            // The choice between those two is `rate_on === dated_on`, not
            // `rate_on === occurred_on`: the question is whether the rate is
            // the very day's or an earlier one, and the day in question is the
            // one the figure is dated at. The contract makes the two equal
            // wherever this branch is reached — a row not assembled from lots
            // is dated on its own occurred_on — so the change moves no row
            // today; it removes a second source for one answer, and the second
            // source is what let a purchase date's rate be compared against
            // the day the paperwork moved.
            //
            // Every one of the three ends in a date, so each answers a date it
            // cannot render — null, whether because none came or because it
            // did not parse — with no tooltip at all: half a sentence ending
            // in a dash claims less than nothing. MoneyCell hands the rate
            // date over already formatted and leaves the decision here,
            // because the neighbouring screen's wordings do not mention a date
            // and must survive its absence; the transfer's own date is
            // formatted beside it, from the field that sentence is about.
            const purchaseDate = inBase ? formatDate(inBase.dated_on) : "";
            const convertedTitle = (rateDate: string | null) => {
              // Unreachable — MoneyCell asks only when it is showing the
              // converted figure, which came from this very object — and
              // silence rather than a guess if it ever is reached.
              if (!inBase) return undefined;
              if (operation.assembled_from_lots) {
                return purchaseDate
                  ? t("operations.convertedAtPurchaseDates", { date: purchaseDate })
                  : undefined;
              }
              if (!rateDate) return undefined;
              return inBase.rate_on === inBase.dated_on
                ? t("operations.convertedAtDate", { date: rateDate })
                : t("operations.convertedAtEarlierDate", { date: rateDate });
            };
            const unconvertedTitle = rowGapTitle(t, operation.in_base_gap);
            return (
              <TableRow key={operation.id}>
                <TableCell className="whitespace-nowrap">{formatDate(operation.occurred_on)}</TableCell>
                <TableCell>
                  <div className="flex flex-wrap items-center gap-1">
                    <Badge variant="secondary">{t(`operationTypes.${operation.type}`)}</Badge>
                    <SourceBadge operation={operation} />
                    <TradingModeBadge operation={operation} />
                  </div>
                </TableCell>
                <TableCell>
                  {instrumentName(operation.instrument_id)}
                  {/* THE BROKER'S OWN WORDS, WHICH THE JOURNAL HAS ALWAYS
                      STORED AND THIS SCREEN NEVER SHOWED. A type is a category
                      and a note is the event: «Погашение Инарктика 001Р-01»
                      says which bond ran out, where the badge above can only
                      say that something was redeemed. It sits under the
                      instrument rather than in a column of its own — most rows
                      have none, and an empty seventh column would cost every
                      row width to serve a few.

                      Rendered as the broker (or the person) wrote it, with no
                      interpretation: this is the one field on the row that is
                      not a figure and not a code, and inventing wording for it
                      is exactly what the type badge already does. */}
                  {operation.note && (
                    <div
                      data-testid="operation-note"
                      className="text-xs text-muted-foreground"
                    >
                      {operation.note}
                    </div>
                  )}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  // The tooltip sits on the whole cell, not just the price
                  // number: "quantity ×" is most of the cell's width for a
                  // large quantity ("100 ×" beside a one- or two-digit
                  // price), and a title on the number alone left that area
                  // dead to the mouse — the explanatory sentence's only hover
                  // target was the smaller half of what it explains.
                  title={
                    operation.quantity && operation.price
                      ? priceTitle(instrumentOf(operation.instrument_id))
                      : undefined
                  }
                >
                  {operation.quantity && operation.price ? (
                    <>
                      {operation.quantity} ×{" "}
                      <span data-testid="operation-price">
                        {/* THE CURRENCY IS THE OPERATION'S, and it is on the
                            number rather than left to the row (#114). The
                            price is money per unit in Operation.currency —
                            the currency amount_minor and fee_minor are in,
                            said so in the contract — and it is published
                            exactly as recorded: the server publishes no
                            converted twin for it (OperationInBase carries an
                            amount and a fee and no price), so no display mode
                            can convert it and none does. In the base-currency
                            mode the two cells to the right are therefore
                            roubles wherever a rate allowed while this one is
                            still dollars, and a reader taking its currency
                            from them takes the wrong one.

                            READ OFF THE OPERATION, never off `baseCurrency`
                            or off the resolved amount beside it: those answer
                            which currency the row's OTHER figures ended up
                            in, and that is a different question. On a row the
                            server could not convert they fall back to this
                            one's currency and the two agree, for a reason
                            that has nothing to do with the price — which is
                            how a client reading them for this answer gets it
                            right until the day it does not.

                            SAID ON EVERY ROW, not only where the row's other
                            figures differ from it. A sign that came and went
                            with the toggle would be two renderings of one
                            number, each correct only in the mode it was last
                            read in — the same decision the positions screen's
                            quote made in #76, and formatPriceIn is the helper
                            it added.

                            The number itself renders as before: a thousands
                            separator, two fraction digits, and the sub-cent
                            branch that keeps a $0,0025 quote from printing as
                            «0,00» (#30) — formatPrice and formatPriceIn share
                            one parse, so a price does not look like two
                            things on two screens and #30 cannot be reopened
                            by #114's own fix. Null for anything outside a plain
                            non-negative decimal, which nothing on the wire
                            should be; the raw value is shown then rather than
                            dropped, because an unstyled number is still the
                            operation's own datum and a dash in its place
                            would hide it — and it is shown WITHOUT a currency,
                            since a sign on a string this program could not
                            read as a price at all would dress up a value it
                            never checked. */}
                        {formatPriceIn(operation.price, operation.currency) ?? operation.price}
                      </span>
                    </>
                  ) : (
                    "—"
                  )}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  <MoneyCell
                    resolved={resolvedAmount}
                    className={signClass(resolvedAmount.amountMinor)}
                    notConvertedTitle={unconvertedTitle}
                    convertedTitle={convertedTitle}
                    // Only the amount, and only on the rows whose amount is a
                    // cost basis. The fee below is a broker's charge on the
                    // day it was charged, no rule picked it out of anything,
                    // and the rows around this one are money that moved.
                    caveatTitle={wasAssembledFromLots(operation) ? costBasisTitle : undefined}
                    testId="operation-amount"
                  />
                </TableCell>
                <TableCell className="text-right tabular-nums text-muted-foreground">
                  {/* A zero fee is genuinely nothing, in any currency — there is
                      no figure to convert and the dash stays a dash. */}
                  {operation.fee_minor > 0 ? (
                    <MoneyCell
                      resolved={resolvedFee}
                      notConvertedTitle={unconvertedTitle}
                      convertedTitle={convertedTitle}
                      testId="operation-fee"
                    />
                  ) : (
                    "—"
                  )}
                </TableCell>
                {/* The cell itself stays whenever the column does, so an
                    imported row does not pull the ones below it a column to the
                    left; what goes is the action inside it, on a row the server
                    would refuse to delete (see isImported). */}
                {canDelete && (
                  <TableCell>
                    {!isImported(operation) && (
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={t("operations.delete")}
                        onClick={() => setDeleteTarget(operation)}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    )}
                  </TableCell>
                )}
              </TableRow>
            );
          })}
        </TableBody>
      </Table>

      {canLoadMore && (
        <Button
          variant="outline"
          disabled={operations.isFetchingNextPage}
          onClick={() => void operations.fetchNextPage()}
        >
          {operations.isFetchingNextPage ? t("app.loading") : t("operations.loadMore")}
        </Button>
      )}

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteTarget(null);
            deleteOperation.reset();
          }
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t("operations.delete")}</DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground">
            {t("operations.deleteConfirm", {
              date: deleteTarget ? formatDate(deleteTarget.occurred_on) : "",
            })}
          </p>
          {deleteOperation.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(deleteOperation.error)
                  ? t("operations.deleteConflict")
                  : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => {
                // Reset here too, not just in onOpenChange: Radix only calls
                // onOpenChange for its own dismiss triggers (Escape, overlay
                // click, DialogClose), not when this plain button flips our
                // `open` prop by clearing deleteTarget. Without this, a
                // failed attempt's error would leak into the next operation's
                // dialog.
                setDeleteTarget(null);
                deleteOperation.reset();
              }}
            >
              {t("common.cancel")}
            </Button>
            <Button
              variant="destructive"
              disabled={deleteOperation.isPending}
              onClick={confirmDelete}
            >
              {t("operations.delete")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
