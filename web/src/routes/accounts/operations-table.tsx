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
import {
  useOperations,
  useDeleteOperation,
  isConflict,
  type Operation,
} from "@/api/operations";
import { useInstruments } from "@/api/instruments";

const PAGE_SIZE = 50;

export function OperationsTable({
  accountId,
  canDelete,
  mode,
  baseCurrency,
}: {
  accountId: string;
  // Delete action is editor+ (owner/editor); viewers never see it.
  canDelete: boolean;
  mode: DisplayCurrencyMode;
  // The space's base currency (SessionInfo.base_currency) — needed to tell
  // "already in base, nothing to convert" apart from "no fx rate for that
  // date" when an operation's in_base is null (see resolveDisplayAmount).
  baseCurrency: string;
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
  const notConvertedTitle = t("operations.notConverted");

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
            // in_base.rate_on is the nearest rate on or before occurred_on
            // (see FxRateOn in the backend), not necessarily a rate dated
            // occurred_on itself — weekends/holidays structurally never get
            // their own backfilled rate. Claiming "on the operation's date"
            // when the two dates differ would contradict the Date column
            // right next to it, so that wording is only used when they
            // actually match; otherwise the honest fallback wording is used.
            const convertedTitle =
              operation.in_base?.rate_on === operation.occurred_on
                ? (date: string) => t("operations.convertedAtDate", { date })
                : (date: string) => t("operations.convertedAtEarlierDate", { date });
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
                    notConvertedTitle={notConvertedTitle}
                    convertedTitle={convertedTitle}
                    testId="operation-amount"
                  />
                </TableCell>
                <TableCell className="text-right tabular-nums text-muted-foreground">
                  {/* A zero fee is genuinely nothing, in any currency — there is
                      no figure to convert and the dash stays a dash. */}
                  {operation.fee_minor > 0 ? (
                    <MoneyCell
                      resolved={resolvedFee}
                      notConvertedTitle={notConvertedTitle}
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
