import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Checkbox } from "@/components/ui/checkbox";
import { formatDate, formatDateTime } from "@/lib/dates";
import { useUnparsed } from "@/api/connections";
import { useRemoveExplanation } from "@/api/explanations";
import { ExplainDialog } from "./explain-dialog";

// UnparsedList shows the broker operations this program could not turn into
// journal entries: what the broker said, why it was refused, and the raw JSON
// folded away. Each is an operation the positions and profit do not account
// for.
export function UnparsedList({ connectionId }: { connectionId: string }) {
  const { t } = useTranslation();
  // Picked rows as (link, content key). One operation belongs to one account,
  // so a selection stays within one link, the first pick's.
  const [pickedLink, setPickedLink] = useState<string | null>(null);
  const [picked, setPicked] = useState<string[]>([]);
  const [explaining, setExplaining] = useState(false);
  const removeExplanation = useRemoveExplanation(connectionId);
  // The same query the reconcile panel's counter reads, by the same key — one
  // request, and one answer that both places state the same way.
  const unparsed = useUnparsed(connectionId);
  const list = unparsed.data?.pages.flatMap((page) => page.operations) ?? [];
  const clearPicks = () => {
    setPicked([]);
    setPickedLink(null);
  };
  const toggle = (linkId: string, contentKey: string) => {
    if (picked.includes(contentKey)) {
      const left = picked.filter((k) => k !== contentKey);
      setPicked(left);
      if (left.length === 0) setPickedLink(null);
      return;
    }
    // A row of another account starts a new selection, visibly.
    if (pickedLink !== null && pickedLink !== linkId) {
      setPicked([contentKey]);
      setPickedLink(linkId);
      return;
    }
    setPicked([...picked, contentKey]);
    setPickedLink(linkId);
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("connections.detail.unparsed.title")}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3">
        {/* A sentence about the rows, printed only when there are rows. */}
        {/* Each sentence only over rows it is true of: the first is false of an
           explained row, whose operation is counted. */}
        {list.some((operation) => operation.explained_by == null) && (
          <p className="text-sm text-muted-foreground">{t("connections.detail.unparsed.intro")}</p>
        )}
        {list.some((operation) => operation.explained_by != null) && (
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.unparsed.introExplained")}
          </p>
        )}
        {unparsed.isPending && (
          <p className="text-sm text-muted-foreground">{t("app.loading")}</p>
        )}
        {unparsed.isError && (
          <Alert variant="destructive">
            <AlertDescription>{t("app.error")}</AlertDescription>
          </Alert>
        )}
        {/* «Неразобранных операций нет», true even for a connection that never
           synced. */}
        {!unparsed.isPending && !unparsed.isError && list.length === 0 && (
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.unparsed.empty")}
          </p>
        )}
        {/* Offered only with something to explain and an account to write to;
           the count is on the button because one operation will stand for
           these rows. */}
        {picked.length > 0 && (
          <div className="flex items-center gap-3">
            <Button size="sm" onClick={() => setExplaining(true)}>
              {t("connections.detail.explain.action", { n: picked.length })}
            </Button>
            <Button size="sm" variant="ghost" onClick={clearPicks}>
              {t("connections.detail.explain.clearSelection")}
            </Button>
          </div>
        )}
        {pickedLink !== null && (
          <ExplainDialog
            open={explaining}
            onOpenChange={setExplaining}
            connectionId={connectionId}
            linkId={pickedLink}
            contentKeys={picked}
            onExplained={clearPicks}
          />
        )}
        {list.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-8" />
                <TableHead>{t("connections.detail.unparsed.columns.occurredAt")}</TableHead>
                <TableHead>{t("connections.detail.unparsed.columns.type")}</TableHead>
                <TableHead className="text-right">
                  {t("connections.detail.unparsed.columns.amount")}
                </TableHead>
                <TableHead>{t("connections.detail.unparsed.columns.reason")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((operation) => (
                <TableRow key={operation.id}>
                  <TableCell>
                    {/* An explained row cannot be picked: take its answer back first
                       («Снять»). */}
                    {operation.explained_by == null && (
                      <Checkbox
                        aria-label={t("connections.detail.explain.pick")}
                        checked={picked.includes(operation.content_key)}
                        onCheckedChange={() => toggle(operation.link_id, operation.content_key)}
                      />
                    )}
                  </TableCell>
                  <TableCell className="whitespace-nowrap">
                    <div className="grid gap-0.5">
                      <span>{formatDateTime(operation.occurred_at)}</span>
                      {/* A row the broker stopped returning stays, with the sync that first
                         missed it. */}
                      {operation.disappeared_at != null && (
                        <span className="text-xs text-muted-foreground">
                          {t("connections.detail.unparsed.disappeared", {
                            at: formatDateTime(operation.disappeared_at),
                          })}
                        </span>
                      )}
                    </div>
                  </TableCell>
                  <TableCell>
                    {/* The broker's type word, verbatim: this program has no rule for it,
                       so a Russian name would claim understanding. */}
                    <div className="grid gap-0.5">
                      <code className="text-xs">{operation.op_type}</code>
                      {/* Where the broker says it happened: the code as sent, a word only
                         where sourced; empty for money in and out. */}
                      {operation.class_code !== "" && (
                        <span
                          data-testid="mirror-trading-mode"
                          className="text-xs text-muted-foreground"
                          title={t(
                            `tradingModeTitles.${
                              operation.trading_mode_kind &&
                              operation.trading_mode_kind !== "unknown"
                                ? operation.trading_mode_kind
                                : "unknown"
                            }`,
                          )}
                        >
                          {operation.trading_mode_kind &&
                          operation.trading_mode_kind !== "unknown"
                            ? `${t(`tradingModes.${operation.trading_mode_kind}`)} · ${operation.class_code}`
                            : operation.class_code}
                        </span>
                      )}
                      {operation.description !== "" && (
                        <span className="text-xs text-muted-foreground">
                          {operation.description}
                        </span>
                      )}
                    </div>
                  </TableCell>
                  {/* The broker's amount as it arrived: the money formatter would round
                     away what made it unreadable. */}
                  <TableCell className="text-right tabular-nums whitespace-nowrap">
                    {operation.payment} {operation.currency}
                  </TableCell>
                  {/* Wrapped: this cell holds sentences. */}
                  <TableCell className="min-w-72 whitespace-normal">
                    <div className="grid gap-1">
                      {/* An explained row has no reason; it shows the owner's answer, its
                         operation and how to take it back, decided by explained_by, not by
                         an empty reason. */}
                      {operation.explained_by != null ? (
                        <>
                          <span>
                            {t("connections.detail.explain.explainedBy", {
                              type: t(`operationTypes.${operation.explained_by.operation_type}`),
                              date: formatDate(operation.explained_by.operation_on),
                            })}
                          </span>
                          {/* An explained row the broker stopped returning may be an
                             unrecognized rewrite, now in the journal beside the owner's
                             operation (#196). */}
                          {operation.disappeared_at != null && (
                            <span className="text-xs text-amber-700 dark:text-amber-400">
                              {t("connections.detail.explain.explainedGone")}
                            </span>
                          )}
                          <div>
                            <Button
                              size="sm"
                              variant="ghost"
                              disabled={removeExplanation.isPending}
                              onClick={() => {
                                const id = operation.explained_by?.id;
                                if (id !== undefined) removeExplanation.mutate(id);
                              }}
                            >
                              {t("connections.detail.explain.remove")}
                            </Button>
                            {/* Said beside the button: the journal entry goes too. */}
                            <p className="text-xs text-muted-foreground">
                              {t("connections.detail.explain.removeHint")}
                            </p>
                          </div>
                        </>
                      ) : (
                        <span>{t(`connections.unparsedReasons.${operation.reason}`)}</span>
                      )}
                      {/* The refuser's own words about this row, shown under the code's
                         Russian name and never read. Untranslated, like the broker's type:
                         part of the record, not something this program says. It is a
                         field of a successful answer (TinvestUnparsedOperation.detail), not
                         an error message, so the i18n rule does not forbid it. Printed
                         only when non-empty. */}
                      {operation.detail !== "" && (
                        <span className="text-xs text-muted-foreground">{operation.detail}</span>
                      )}
                      <details>
                        <summary className="cursor-pointer text-xs text-muted-foreground">
                          {t("connections.detail.unparsed.raw")}
                        </summary>
                        <pre className="mt-1 max-w-md overflow-x-auto rounded bg-muted p-2 text-xs">
                          {JSON.stringify(operation.raw, null, 2)}
                        </pre>
                      </details>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {unparsed.hasNextPage && (
          <div>
            <Button
              variant="outline"
              disabled={unparsed.isFetchingNextPage}
              onClick={() => void unparsed.fetchNextPage()}
            >
              {unparsed.isFetchingNextPage ? t("app.loading") : t("connections.detail.loadMore")}
            </Button>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
