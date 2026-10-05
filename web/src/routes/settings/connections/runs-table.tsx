import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
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
import { formatDateTime } from "@/lib/dates";
import {
  useSyncRuns,
  type TinvestLinkedAccount,
  type TinvestSyncRun,
  type TinvestSyncRunStatus,
} from "@/api/connections";

// A switch over the three statuses, so a fourth is a type error.
function runVariant(status: TinvestSyncRunStatus): "default" | "secondary" | "destructive" {
  switch (status) {
    case "ok":
      return "default";
    case "failed":
      return "destructive";
    case "running":
      return "secondary";
  }
}

// A run's counters depend on how it ended; they default to zero and are
// written at close (migration 0014).
//
//   - running: nothing written; "not finished" rather than zeros.
//   - ok: all four measured and shown.
//   - failed: the first three are real (a rolled-back pass read nothing); the
//     fourth may be a failed count's zero, so the failure reason is shown
//     instead.
function RunWork({ run }: { run: TinvestSyncRun }) {
  const { t } = useTranslation();
  if (run.status === "running") {
    return (
      <span className="text-muted-foreground">{t("connections.detail.runs.unfinished")}</span>
    );
  }
  return (
    <div className="grid gap-0.5 text-xs">
      <span>{t("connections.detail.runs.read", { n: run.read_count })}</span>
      <span>{t("connections.detail.runs.added", { n: run.added_count })}</span>
      <span>{t("connections.detail.runs.disappeared", { n: run.disappeared_count })}</span>
      {run.status === "ok" && (
        <span>{t("connections.detail.runs.unparsed", { n: run.unparsed_count })}</span>
      )}
      {run.status === "failed" && (
        <span className="text-destructive">
          {t("connections.detail.runs.failedCause")}: {run.error}
        </span>
      )}
    </div>
  );
}

// RunsTable is the sync log, newest first, paged. The broker account is named
// by joining the run's link_id against the connection's links.
export function RunsTable({
  connectionId,
  links,
}: {
  connectionId: string;
  links: TinvestLinkedAccount[];
}) {
  const { t } = useTranslation();
  const runs = useSyncRuns(connectionId);
  const list = runs.data?.pages.flatMap((page) => page.runs) ?? [];

  // A dash for a link not among the connection's, never another account's
  // name.
  const brokerAccountName = (linkId: string) =>
    links.find((link) => link.link_id === linkId)?.broker_account_name ?? "—";

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("connections.detail.runs.title")}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3">
        {runs.isPending && <p className="text-sm text-muted-foreground">{t("app.loading")}</p>}
        {runs.isError && (
          <Alert variant="destructive">
            <AlertDescription>{t("app.error")}</AlertDescription>
          </Alert>
        )}
        {!runs.isPending && !runs.isError && list.length === 0 && (
          <p className="text-sm text-muted-foreground">{t("connections.detail.runs.empty")}</p>
        )}
        {list.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("connections.detail.runs.columns.startedAt")}</TableHead>
                {/* Qualified: the name was taken when the link was made. */}
                <TableHead title={t("connections.detail.brokerAccountNameFrozen")}>
                  {t("connections.detail.runs.columns.account")}
                </TableHead>
                <TableHead>{t("connections.detail.runs.columns.trigger")}</TableHead>
                <TableHead>{t("connections.detail.runs.columns.status")}</TableHead>
                <TableHead>{t("connections.detail.runs.columns.work")}</TableHead>
                <TableHead>{t("connections.detail.runs.columns.reconcile")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((run) => (
                <TableRow key={run.id}>
                  <TableCell className="whitespace-nowrap">
                    {formatDateTime(run.started_at)}
                  </TableCell>
                  <TableCell>{brokerAccountName(run.link_id)}</TableCell>
                  <TableCell>{t(`connections.triggers.${run.trigger}`)}</TableCell>
                  <TableCell>
                    <Badge variant={runVariant(run.status)}>
                      {t(`connections.runStatuses.${run.status}`)}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <RunWork run={run} />
                  </TableCell>
                  <TableCell className="text-xs">
                    {/* «Не проверено» is drawn as itself; the count only for `mismatched`,
                       where an empty list means "found nothing". */}
                    <div className="grid gap-0.5">
                      <span>{t(`connections.reconcileStatuses.${run.reconcile_status}`)}</span>
                      {run.reconcile_status === "mismatched" && (
                        <span className="text-muted-foreground">
                          {t("connections.detail.runs.mismatchCount", {
                            n: run.mismatches.length,
                          })}
                        </span>
                      )}
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {/* The server's answer, not the page length (#86). */}
        {runs.hasNextPage && (
          <div>
            <Button
              variant="outline"
              disabled={runs.isFetchingNextPage}
              onClick={() => void runs.fetchNextPage()}
            >
              {runs.isFetchingNextPage ? t("app.loading") : t("connections.detail.loadMore")}
            </Button>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
