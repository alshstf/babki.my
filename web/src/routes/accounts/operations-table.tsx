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
import { signClass } from "@/lib/money";
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
  type Operation,
} from "@/api/operations";
import { useInstruments } from "@/api/instruments";
import type { CostBasisRules } from "@/api/tax-residencies";

const PAGE_SIZE = 50;

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
  // the journal response is a bare array with nowhere to carry it (see the
  // API contract). Undefined while the session is still loading — the caveat
  // simply waits rather than guessing.
  costBasisRules?: CostBasisRules;
}) {
  const { t } = useTranslation();
  // "Show more" grows the fetch window (limit += 50, offset stays 0) instead
  // of accumulating separate offset pages client-side. The backend returns a
  // stable occurred_on/created_at DESC order, so re-fetching [0, limit) on
  // each click always yields the same accumulated list with no client-side
  // merge/dedup bookkeeping. Downside: once total operations is an exact
  // multiple of PAGE_SIZE, "load more" stays visible for one extra (empty)
  // click — acceptable for MVP journal sizes.
  const [limit, setLimit] = useState(PAGE_SIZE);
  const operations = useOperations(accountId, limit, 0);
  // Instrument catalog for name lookup, capped at 50 (see useInstruments) —
  // enough for the MVP catalog. If it ever grows past 50, an operation whose
  // instrument isn't in this page falls back to an id suffix below instead
  // of failing to render; not worth an issue while the catalog stays small.
  const instruments = useInstruments("");
  const deleteOperation = useDeleteOperation();
  const [deleteTarget, setDeleteTarget] = useState<Operation | null>(null);
  const list = operations.data ?? [];

  // The journal reports its own currencies to the screen-wide counter that
  // decides whether the header's display-currency toggle is shown. It reports
  // separately from the screen component (see detail.tsx) because it owns its
  // own query, and its currencies can be ones nothing else on the screen
  // knows about: a foreign-currency operation on a base-currency account is
  // otherwise invisible to the counter, leaving the user with amounts they
  // cannot switch. Only currencies in the currently loaded window (`list`,
  // capped by `limit`) are counted — if the sole foreign-currency operation
  // sits past row 50, the toggle won't appear until "Show more" is clicked.
  // That's consistent with what the table actually shows, so it's accepted
  // rather than worked around. Must run unconditionally, before the early
  // returns below, per the Rules of Hooks.
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
  // Two different conditions leave a row unconverted, exactly as on the
  // positions screen, and they are not the same news. A missing fx rate is a
  // gap the instance's own backfill closes — the ruble figure appears later.
  // A transfer whose purchase dates were never recorded (has_undated_lots,
  // see the API contract) never converts, and here "нет курса на дату
  // операции" is not merely unhelpful but false: the transfer's own date
  // usually has a rate, it is just not a rate that may value a basis assembled
  // on other days. Naming it would blame a cause that is not the cause and
  // promise a number that will never come. Both are per-row, hence resolved
  // inside the map below.
  const notConvertedTitle = t("operations.notConverted");
  const undatedLotsTitle = t("operations.notConvertedUndatedLots");
  // The caveat that a cost basis here was picked by a queue that is not the
  // owner's country's. It hangs on the amount cells whose figure actually IS
  // one, and nowhere else. It used to be a banner over the whole table, which
  // put "computed by a rule that is not your country's" above fifty deposits,
  // purchases and dividends it says nothing about, and — since the positions
  // above render the same banner — printed the identical paragraph twice on
  // one screen. Undefined when the session is still loading or when the
  // country's rule IS what is computed (see costBasisCaveat).
  const costBasisTitle = costBasisRules ? costBasisCaveat(t, costBasisRules) : undefined;

  const instrumentName = (instrumentId: string | null | undefined) => {
    if (!instrumentId) return "—";
    const found = instruments.data?.find((instrument) => instrument.id === instrumentId);
    return found ? found.name : `#${instrumentId.slice(-8)}`;
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

  const canLoadMore = list.length === limit;

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
            // and rate_on names only the newest of them. That case must be
            // checked FIRST, because neither of the other two wordings is true
            // of it and a date comparison cannot tell: rate_on happens to equal
            // occurred_on whenever the last purchase was made on the transfer
            // day, and differs from it otherwise, so relying on the dates picks
            // one false sentence or the other. On the demo data it read "there
            // is no rate for the operation's date — converted at the nearest,
            // 15.06.2026" about a figure assembled from two rates, on a day
            // whose rate exists and was deliberately not used.
            //
            // Otherwise rate_on is the nearest rate on or before occurred_on
            // (see FxRateOn in the backend), not necessarily a rate dated
            // occurred_on itself — weekends/holidays structurally never get
            // their own backfilled rate. Claiming "on the operation's date"
            // when the two dates differ would contradict the Date column
            // right next to it, so that wording is only used when they
            // actually match; otherwise the honest fallback wording is used.
            //
            // All three name the rate date, so all three answer a date they
            // cannot render — null, whether because none came or because it
            // did not parse — with no tooltip at all: half a sentence ending
            // in a dash claims less than nothing. MoneyCell hands the decision
            // over rather than making it, because the neighbouring screen's
            // wordings do not mention a date and must survive its absence.
            const convertedTitle = (date: string | null) => {
              if (!date) return undefined;
              if (operation.assembled_from_lots) {
                return t("operations.convertedAtPurchaseDates", { date });
              }
              return operation.in_base?.rate_on === operation.occurred_on
                ? t("operations.convertedAtDate", { date })
                : t("operations.convertedAtEarlierDate", { date });
            };
            const unconvertedTitle = operation.has_undated_lots
              ? undatedLotsTitle
              : notConvertedTitle;
            return (
              <TableRow key={operation.id}>
                <TableCell className="whitespace-nowrap">{formatDate(operation.occurred_on)}</TableCell>
                <TableCell>
                  <Badge variant="secondary">{t(`operationTypes.${operation.type}`)}</Badge>
                </TableCell>
                <TableCell>{instrumentName(operation.instrument_id)}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {operation.quantity && operation.price
                    ? `${operation.quantity} × ${operation.price}`
                    : "—"}
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
          disabled={operations.isFetching}
          onClick={() => setLimit((current) => current + PAGE_SIZE)}
        >
          {operations.isFetching ? t("app.loading") : t("operations.loadMore")}
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
