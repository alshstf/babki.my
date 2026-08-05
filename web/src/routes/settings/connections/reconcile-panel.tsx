import { useTranslation } from "react-i18next";
import { CheckCircle2, CircleDashed, TriangleAlert } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDateTime } from "@/lib/dates";
import { useUnparsed, type TinvestReconcileSnapshot } from "@/api/connections";

// What the panel is going to say, decided in one place from the one field that
// is entitled to decide it.
//
// THE SNAPSHOT'S PRESENCE IS NOT THE VERDICT — its `status` is. The API
// contract says a snapshot is built only from runs that actually made a check,
// so `not_checked` cannot arrive inside one; if it ever did, the object would
// be a check that says of itself that it never happened, and this maps it to
// «не проверено» rather than letting a rendered timestamp imply a check behind
// it. Absence maps there too, and for the plainer reason: no run ever
// reconciled at all.
//
// «Не проверено» IS NOT «СХОДИТСЯ» AND NOT «РАСХОЖДЕНИЙ НЕТ». A run that never
// reconciled makes no claim about the broker whatsoever, and the tick below
// belongs to `matched` alone — this project has drawn a caption over a figure
// that did not support it four times, and this is the caption most able to do
// it again, because an empty mismatch list looks exactly like agreement.
type Verdict = "not_checked" | "matched" | "mismatched";

function verdictOf(snapshot: TinvestReconcileSnapshot | null | undefined): Verdict {
  if (!snapshot) return "not_checked";
  switch (snapshot.status) {
    case "matched":
      return "matched";
    case "mismatched":
      return "mismatched";
    case "not_checked":
      return "not_checked";
  }
}

// ReconcilePanel draws the connection's most recent check against the broker:
// the verdict, what differed if anything did, and — beside it — how many of the
// broker's operations this program still could not read, since an instrument
// difference is usually a symptom of exactly those.
export function ReconcilePanel({
  connectionId,
  snapshot,
}: {
  connectionId: string;
  snapshot: TinvestReconcileSnapshot | null | undefined;
}) {
  const { t } = useTranslation();
  const verdict = verdictOf(snapshot);
  // The same query the list below this panel runs, by the same key: react-query
  // hands both callers one cached answer and makes one request, and — what
  // matters more than the request — the counter here and the rows down there
  // can never state different things about one list. A separate "how many are
  // there" endpoint would be a second computation of one figure, which this
  // project's own rule says will eventually disagree with the first.
  const unparsed = useUnparsed(connectionId);
  const loadedUnparsed =
    unparsed.data?.pages.reduce((rows, page) => rows + page.operations.length, 0) ?? 0;

  // Only ever as much as was actually fetched. `hasNextPage` is the server's
  // own `has_more` (see useUnparsed), so when there is more behind the page the
  // count is published as a floor and not as a total — writing the loaded rows
  // as though they were all of them would be a figure nobody measured.
  const unparsedCaption = (() => {
    if (unparsed.isPending) return null;
    if (unparsed.isError) return t("connections.detail.reconcile.unparsedUnknown");
    if (unparsed.hasNextPage)
      return t("connections.detail.reconcile.unparsedAtLeast", { n: loadedUnparsed });
    if (loadedUnparsed === 0) return t("connections.detail.reconcile.unparsedNone");
    return t("connections.detail.reconcile.unparsedCount", { n: loadedUnparsed });
  })();

  // Null when the instant will not parse — half a sentence ending in nothing
  // claims less than nothing, so the panel says that it could not read the time
  // rather than printing «Проверено ».
  const checkedAt = (() => {
    if (!snapshot) return null;
    const at = formatDateTime(snapshot.at);
    return at
      ? t("connections.detail.reconcile.checkedAt", { time: at })
      : t("connections.detail.reconcile.checkedAtUnreadable");
  })();

  const mismatches = snapshot?.mismatches ?? [];
  const hasInstrumentRow = mismatches.some((m) => m.kind === "instrument");
  const hasUnsupportedRow = mismatches.some((m) => m.kind === "unsupported");

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("connections.detail.reconcile.title")}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        {/* Three states, three looks. The tick is `matched`'s alone. */}
        {verdict === "not_checked" && (
          <div className="grid gap-1">
            <p className="flex items-center gap-2 font-medium">
              <CircleDashed className="size-4 text-muted-foreground" />
              {t("connections.detail.reconcile.notChecked")}
            </p>
            <p className="text-sm text-muted-foreground">
              {t("connections.detail.reconcile.notCheckedBody")}
            </p>
          </div>
        )}
        {verdict === "matched" && (
          <div className="grid gap-1">
            <p className="flex items-center gap-2 font-medium">
              <CheckCircle2 className="size-4 text-emerald-600" />
              {t("connections.detail.reconcile.matched")}
            </p>
            <p className="text-sm text-muted-foreground">
              {t("connections.detail.reconcile.matchedBody")}
            </p>
            {checkedAt && <p className="text-xs text-muted-foreground">{checkedAt}</p>}
          </div>
        )}
        {verdict === "mismatched" && (
          <div className="grid gap-1">
            <p className="flex items-center gap-2 font-medium">
              <TriangleAlert className="size-4 text-destructive" />
              {t("connections.detail.reconcile.mismatched")}
            </p>
            <p className="text-sm text-muted-foreground">
              {t("connections.detail.reconcile.mismatchedBody")}
            </p>
            {checkedAt && <p className="text-xs text-muted-foreground">{checkedAt}</p>}
          </div>
        )}

        {/* Rendered off the LIST rather than off the verdict, so the table and
            the word above it cannot come apart: the contract derives one from
            the other on the server (empty exactly when `matched`), and reading
            the list here keeps that single fact single. */}
        {mismatches.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("connections.detail.reconcile.columns.label")}</TableHead>
                <TableHead>{t("connections.detail.reconcile.columns.kind")}</TableHead>
                <TableHead className="text-right">
                  {t("connections.detail.reconcile.columns.broker")}
                </TableHead>
                <TableHead className="text-right">
                  {t("connections.detail.reconcile.columns.journal")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {mismatches.map((mismatch, index) => (
                // The rows carry no id of their own — a currency row and an
                // unmatched broker position both have a null instrument_id —
                // and the list is a snapshot that is never reordered or edited
                // in place, so the position in it is the only key there is.
                <TableRow key={`${mismatch.kind}-${mismatch.label}-${index}`}>
                  <TableCell>{mismatch.label}</TableCell>
                  <TableCell>
                    {/* Three different pieces of news, and the contract keeps
                        them apart precisely so a reader is not sent hunting
                        for missing operations over an asset this program was
                        never going to hold. */}
                    <Badge variant="secondary">
                      {t(`connections.mismatchKinds.${mismatch.kind}`)}
                    </Badge>
                  </TableCell>
                  {/* Printed as they arrived. Both are decimal strings — units
                      of a security on one row, an amount of a currency on the
                      next — and this screen has no business rounding either. */}
                  <TableCell className="text-right tabular-nums">{mismatch.broker}</TableCell>
                  <TableCell className="text-right tabular-nums">{mismatch.journal}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}

        {hasInstrumentRow && (
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.reconcile.instrumentNote")}
          </p>
        )}
        {hasUnsupportedRow && (
          <Alert>
            <AlertDescription>
              {t("connections.detail.reconcile.unsupportedNote")}
            </AlertDescription>
          </Alert>
        )}

        {unparsedCaption && (
          <p className="text-sm text-muted-foreground">{unparsedCaption}</p>
        )}
      </CardContent>
    </Card>
  );
}
