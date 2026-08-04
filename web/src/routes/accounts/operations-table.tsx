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
import { formatPrice, signClass } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { resolveDisplayAmount } from "@/lib/display-amount";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import { useReportScreenCurrencies } from "@/lib/screen-currencies";
import { MoneyCell } from "@/components/money-cell";
import { costBasisCaveat } from "@/components/cost-basis-notice";
import {
  useOperations,
  useDeleteOperation,
  isConflict,
  JOURNAL_PAGE_SIZE,
  type Operation,
  type OperationInBaseGap,
} from "@/api/operations";
import { useInstruments, type Instrument } from "@/api/instruments";
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
// Two published fields answer, both properties of the OPERATION itself and
// both present whether or not in_base is (see Operation.has_undated_lots and
// Operation.assembled_from_lots in the API contract):
//
//   - assembled_from_lots says the amount was built piece by piece out of the
//     purchases behind it, each at its own day's rate. The server sets it
//     from the presence of a stored breakdown, not from the row's type, so a
//     new type that carries one is covered the day it appears.
//   - has_undated_lots says this amount is a cost basis whose purchase dates
//     are not all known — a missing or partial breakdown.
//
// Together they are exhaustive: every transfer_in/transfer_out has EITHER a
// stored breakdown (assembled_from_lots true) OR none/a partial one
// (has_undated_lots true) — a breakdown with one dateless piece among dated
// ones makes both true at once, which is fine, since either alone is enough
// to show the caveat. Neither ordinary operation ever sets either field.
//
// Until #67, assembled_from_lots lived INSIDE in_base and vanished the moment
// the conversion block did — including for the most ordinary reason a
// conversion block can be absent, `currency` already equalling the base
// currency. A transferred parcel of RUB shares in a RUB-based space (the
// product owner's own case) has a complete, dated breakdown and nothing to
// convert, so it used to publish nothing about being a cost basis at all: not
// a missing rate, not a missing date, just silence. Moving the field onto the
// operation removed that hole without adding a client-side list of types.
function publishesACostBasis(operation: Operation): boolean {
  return operation.has_undated_lots || operation.assembled_from_lots;
}

// Runtime backstop for a gap value this build cannot name, and the same one
// the positions screen keeps for the identical reason (see unnameableGap
// there). TypeScript proves the switch below exhaustive over the union it was
// compiled against — a new member of the contract's enum without a case here
// fails to compile, because the argument would no longer narrow to `never` —
// but these values are JSON off the wire, typed by assertion rather than
// validated, so a client running behind the server it talks to can still
// receive a literal outside that union. That must degrade to a sentence which
// is still true, never to a blank tooltip and never to a thrown render.
function unnameableGap<T>(_: never, fallback: T): T {
  return fallback;
}

// The one sentence that captions BOTH money cells of a row, chosen from the
// term the server says it stopped on (Operation.in_base_gap). It is the row's
// only source: has_undated_lots answers the first of these causes on its own
// and cannot disagree with it — both derive from one server-side predicate —
// but a caption assembled from two sources is a caption that will eventually
// be assembled from two sources that have drifted. That field keeps its other
// job on this screen (whether the amount IS a cost basis, which it answers on
// the create and transfer responses too, where no gap is published at all);
// it simply no longer decides what the row says about a missing rate.
//
// Each sentence explains why the whole row carries no base-currency figures,
// which is what makes it true over the amount cell and the fee cell alike:
// in_base is published as a whole or not at all, so a single unvaluable term
// withholds both figures together.
//
// The permanent cause is told apart from the temporary ones in as many words,
// because that is the difference the reader is actually served by. A missing
// rate is a gap the fx backfill closes on its own, after which the ruble
// figure appears; an unrecorded purchase date resolves never, since nobody
// wrote it down and nothing can recover it, and a caption that promised such a
// row a figure would promise one that is not coming.
//
// The wordings are the positions screen's wherever the cause is the same
// («восстановить … уже неоткуда», «Курс появится при обновлении курсов, и …
// посчитается сама»): two screens explaining one condition differently is a
// reader's problem, not a translator's.
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
      // left neither to crash nor to say nothing: the general phrase is vague
      // but true — some rate is missing somewhere — and it is what an older
      // server's payload deserves.
      return t("operations.notConverted");
    default:
      return unnameableGap(gap, t("operations.notConverted"));
  }
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
  // Instrument catalog for name lookup, capped at 50 (see useInstruments) —
  // enough for the MVP catalog. If it ever grows past 50, an operation whose
  // instrument isn't in this page falls back to an id suffix below instead
  // of failing to render; not worth an issue while the catalog stays small.
  const instruments = useInstruments("");
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

  // The catalog entry behind a row, when the loaded page happens to hold it.
  // Undefined for a row with no instrument at all (a deposit, a fee) and for
  // one whose instrument sits past the fiftieth (see useInstruments), so every
  // caller has to work without it.
  const instrumentOf = (instrumentId: string | null | undefined): Instrument | undefined =>
    instrumentId ? instruments.data?.find((instrument) => instrument.id === instrumentId) : undefined;

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
  //   - Put a currency on the number — «950,00 ₽». Rejected as noise: the
  //     amount and the fee in this very row already carry the currency, so the
  //     row reads as money without help, and a price is always denominated in
  //     the operation's own currency.
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
            const resolvedAmount = resolveDisplayAmount(
              mode,
              operation.currency,
              operation.amount_minor,
              baseCurrency,
              operation.in_base?.amount_minor,
              operation.in_base?.rate_on,
            );
            const resolvedFee = resolveDisplayAmount(
              mode,
              operation.currency,
              operation.fee_minor,
              baseCurrency,
              operation.in_base?.fee_minor,
              operation.in_base?.rate_on,
            );
            // Three different things a converted amount can be, and the
            // tooltip has to name the right one — it is read as a statement of
            // fact about the number beside it.
            //
            // A transfer between the family's own accounts carries the cost of
            // shares bought on other days, so the backend converts it piece by
            // piece at the rates of those days (operation.assembled_from_lots)
            // and its headline date is the newest PURCHASE, not the transfer.
            // That case must be checked FIRST, because neither of the other
            // two wordings is true of it and a date comparison cannot tell:
            // the headline rate happens to be dated the transfer day whenever
            // the last purchase was made on it, so relying on the dates picks
            // one false sentence or the other. On the demo data it read "there
            // is no rate for the operation's date — converted at the nearest,
            // 15.06.2026" about a figure assembled from two rates, on a day
            // whose rate exists and was deliberately not used.
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
            const inBase = operation.in_base;
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
                  <Badge variant="secondary">{t(`operationTypes.${operation.type}`)}</Badge>
                </TableCell>
                <TableCell>{instrumentName(operation.instrument_id)}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {operation.quantity && operation.price ? (
                    <>
                      {operation.quantity} ×{" "}
                      <span
                        data-testid="operation-price"
                        title={priceTitle(instrumentOf(operation.instrument_id))}
                      >
                        {/* formatPrice, not the wire string: a thousands
                            separator, two fraction digits, and the sub-cent
                            branch that keeps a $0,0025 quote from printing as
                            «0,00» (#30) — the same rendering the positions
                            screen gives a price, so one number does not look
                            like two things on two screens. It answers null for
                            anything outside a plain non-negative decimal,
                            which nothing on the wire should be; the raw value
                            is shown then rather than dropped, because an
                            unstyled number is still the operation's own datum
                            and a dash in its place would hide it. */}
                        {formatPrice(operation.price) ?? operation.price}
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
                    caveatTitle={publishesACostBasis(operation) ? costBasisTitle : undefined}
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
                {canDelete && (
                  <TableCell>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={t("operations.delete")}
                      onClick={() => setDeleteTarget(operation)}
                    >
                      <Trash2 className="size-4" />
                    </Button>
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
