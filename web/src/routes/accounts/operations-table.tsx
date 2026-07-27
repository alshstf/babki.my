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
import { cn } from "@/lib/utils";
import { formatMinor, signClass } from "@/lib/money";
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
}: {
  accountId: string;
  // Delete action is editor+ (owner/editor); viewers never see it.
  canDelete: boolean;
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

  const list = operations.data ?? [];
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
          {list.map((operation) => (
            <TableRow key={operation.id}>
              <TableCell className="whitespace-nowrap">{operation.occurred_on}</TableCell>
              <TableCell>
                <Badge variant="secondary">{t(`operationTypes.${operation.type}`)}</Badge>
              </TableCell>
              <TableCell>{instrumentName(operation.instrument_id)}</TableCell>
              <TableCell className="text-right tabular-nums">
                {operation.quantity && operation.price
                  ? `${operation.quantity} × ${operation.price}`
                  : "—"}
              </TableCell>
              <TableCell
                className={cn(
                  "text-right tabular-nums",
                  signClass(operation.amount_minor),
                )}
              >
                {formatMinor(operation.amount_minor, operation.currency)}
              </TableCell>
              <TableCell className="text-right tabular-nums text-muted-foreground">
                {operation.fee_minor > 0
                  ? formatMinor(operation.fee_minor, operation.currency)
                  : "—"}
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
          ))}
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
            {t("operations.deleteConfirm", { date: deleteTarget?.occurred_on ?? "" })}
          </p>
          {deleteOperation.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(deleteOperation.error)
                  ? t("operations.deleteConflict")
                  : deleteOperation.error.message}
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
