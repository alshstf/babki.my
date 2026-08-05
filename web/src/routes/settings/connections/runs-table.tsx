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

// A switch over the contract's own three values rather than a two-way test, so
// a fourth one added there arrives here as a type error instead of quietly
// drawing as «всё хорошо».
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

// WHAT A RUN'S COUNTERS MEAN DEPENDS ON HOW IT ENDED, and the three columns of
// the sync_runs table say so plainly (migration 0014): every count defaults to
// zero at INSERT and is written only when the run is closed.
//
//   - `running`: nothing has been written to any of them. Their zero is the
//     column default — a placeholder, not a measurement — so this cell says the
//     run has not finished instead of printing four zeros that would read as
//     «прочитано ноль операций». A run stuck here for good is a process that
//     died mid-sync, and «не закончился» is true of that too.
//   - `ok`: all four were measured and all four are shown.
//   - `failed`: the first three were measured — the mirror pass either wrote
//     its rows or rolled back, and a pass that recorded nothing genuinely read,
//     added and lost nothing (see syncWorker.failed). The FOURTH is not of that
//     kind: it is a count taken at the moment of failure that falls back to
//     zero when the count itself fails, and the row has nowhere to say which of
//     the two a zero is. So the failed run shows why it failed instead — a
//     drawn zero would be a measurement nobody can vouch for.
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

// RunsTable is the connection's sync log, newest first, one page at a time.
//
// `links` comes from the connection this screen already loaded, so the broker
// account behind a run is named by joining the run's own link_id against it —
// the contract publishes the link rather than the account for exactly that, so
// the answer is read off the row instead of being assembled from a second query
// whose data could have moved on.
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

  // A dash rather than an invented name: a run whose link is not among the
  // connection's own is not something this screen can name, and naming it
  // after some other account would be worse than saying nothing.
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
                {/* Qualified for the reason the connection screen's own list
                    qualifies it: the name was taken when the link was made and
                    is not re-read on every sync. */}
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
                    {/* «Не проверено» is one of the three and is drawn as
                        itself: a run that reconciled nothing says so, and an
                        empty mismatch list beside it is not agreement. The
                        count is shown only where the list means «what was
                        found», which the contract says is `mismatched` alone —
                        the same empty list means «nobody looked» under the
                        other two. */}
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
        {/* The server's own answer, never the page's length: an over-large
            `limit` is refused rather than clamped here, so a short page cannot
            be read as the end of the log (#86 on the journal). */}
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
