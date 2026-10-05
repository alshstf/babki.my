import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { CheckCircle2, CircleDashed, TriangleAlert } from "lucide-react";
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
import { useAccounts } from "@/api/accounts";
import { useAddInstrumentByISIN } from "@/api/instruments";
import {
  useTriggerSync,
  useUnparsed,
  type TinvestAccountReconcile,
  type TinvestReconcileMismatch,
  type TinvestReconcileStatus,
} from "@/api/connections";

// A verdict belongs to its account: each sync run checks one broker account,
// and the server publishes one verdict per linked account (TinvestAccountReconcile).
// «Не проверено» is neither agreement nor "no differences"; the tick is
// `matched`'s alone.

// The connection's line, derived from all accounts:
//
//   - "matched": every account checked and agreeing;
//   - "mismatched": at least one differs, whether or not the rest were checked;
//   - "partial": nothing differs where checked, and some were not;
//   - "not_checked": none were checked;
//   - "none": no accounts left (deleting a babki account drops its link).
type Overall = "none" | "not_checked" | "partial" | "matched" | "mismatched";

function overallOf(list: TinvestAccountReconcile[]): Overall {
  if (list.length === 0) return "none";
  if (list.some((r) => r.status === "mismatched")) return "mismatched";
  if (list.every((r) => r.status === "matched")) return "matched";
  if (list.every((r) => r.status === "not_checked")) return "not_checked";
  return "partial";
}

// The broker's passport of a paper that is not ours, under its ticker
// («TECH2» alone says nothing). Each line only where its field is set, so a run
// recorded before passports draws nothing.
function PassportLine({
  mismatch,
}: {
  mismatch: TinvestReconcileMismatch;
}): React.JSX.Element | null {
  const { t } = useTranslation();
  const parts: string[] = [];
  if (mismatch.broker_name) parts.push(mismatch.broker_name);
  if (mismatch.broker_isin) parts.push(mismatch.broker_isin);
  if (mismatch.broker_type) parts.push(t(`instrumentTypes.${mismatch.broker_type}`));
  if (mismatch.broker_currency) parts.push(mismatch.broker_currency);
  if (parts.length === 0) return null;
  return (
    <div className="text-muted-foreground text-xs">
      {t("connections.detail.reconcile.passport", { fields: parts.join(" · ") })}
    </div>
  );
}

// «Завести в каталог по паспорту брокера» creates a catalog row from the
// passport's four fields. Offered only with an ISIN and a type: the
// reconciliation pairs by ISIN, so a row without one would pair with nothing. The
// difference closes only after the next check, so the message names the sync as
// the sync request actually answered, or says it could not be asked.
function AddToCatalogButton({
  connectionId,
  mismatch,
}: {
  connectionId: string;
  mismatch: TinvestReconcileMismatch;
}): React.JSX.Element | null {
  const { t } = useTranslation();
  const add = useAddInstrumentByISIN();
  const triggerSync = useTriggerSync();

  const isin = mismatch.broker_isin;
  const type = mismatch.broker_type;
  const currency = mismatch.broker_currency;
  if (!isin || !type || !currency) return null;

  const message = (() => {
    if (add.isError) return t("connections.detail.reconcile.catalog.failed");
    if (!add.data) return null;
    if (!add.data.created)
      return t("connections.detail.reconcile.catalog.alreadyThere", {
        name: add.data.instrument.name,
      });
    if (triggerSync.isPending) return t("connections.detail.reconcile.catalog.created");
    if (triggerSync.isError) return t("connections.detail.reconcile.catalog.createdSyncRefused");
    if (triggerSync.data)
      return triggerSync.data.queued
        ? t("connections.detail.reconcile.catalog.createdSyncQueued")
        : t("connections.detail.reconcile.catalog.createdSyncAlreadyQueued");
    return t("connections.detail.reconcile.catalog.created");
  })();

  return (
    <div className="grid gap-1">
      <Button
        variant="outline"
        size="sm"
        disabled={add.isPending || add.isSuccess}
        onClick={() =>
          add.mutate(
            {
              isin,
              type,
              // The broker's naming is the catalog ticker and the passport's name the
              // name; the label stands in for a missing name, which the catalog refuses.
              ticker: mismatch.label,
              name: mismatch.broker_name ?? mismatch.label,
              currency,
            },
            {
              // Only a newly created row is worth a re-check.
              onSuccess: (result) => {
                if (result.created) triggerSync.mutate(connectionId);
              },
            },
          )
        }
      >
        {t("connections.detail.reconcile.catalog.button")}
      </Button>
      {message && <div className="text-muted-foreground text-xs">{message}</div>}
    </div>
  );
}

// One account's differences, both figures, never just the gap.
function MismatchTable({
  connectionId,
  mismatches,
}: {
  connectionId: string;
  mismatches: TinvestReconcileMismatch[];
}) {
  const { t } = useTranslation();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>
            {t("connections.detail.reconcile.columns.label")}
          </TableHead>
          <TableHead>
            {t("connections.detail.reconcile.columns.kind")}
          </TableHead>
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
          // Rows have no id (currency rows and unmatched positions lack an
          // instrument_id) and the list is never reordered, so the index is the
          // key.
          <TableRow key={`${mismatch.kind}-${mismatch.label}-${index}`}>
            <TableCell>
              <div className="grid gap-1">
                <div>{mismatch.label}</div>
                <PassportLine mismatch={mismatch} />
                {/* A question, not a finding: set only when the quantities differ by a
                   whole factor and the registry has no split for it (the owner's AMZN 1
                   against 20, NVDA 3 against 30). A missed purchase leaves the same shape,
                   so nothing to press; recording an event is done on the catalog
                   screen. */}
                {mismatch.split_hint_factor != null && (
                  <div
                    className="text-xs text-muted-foreground"
                    data-testid="mismatch-split-hint"
                  >
                    {t("connections.detail.reconcile.splitHint", {
                      factor: mismatch.split_hint_factor,
                    })}
                  </div>
                )}
                {mismatch.kind === "unknown_security" && (
                  <AddToCatalogButton
                    connectionId={connectionId}
                    mismatch={mismatch}
                  />
                )}
              </div>
            </TableCell>
            <TableCell>
              {/* Three different pieces of news, kept apart by the contract. */}
              <Badge variant="secondary">
                {t(`connections.mismatchKinds.${mismatch.kind}`)}
              </Badge>
            </TableCell>
            <TableCell className="text-right tabular-nums">
              {mismatch.broker}
            </TableCell>
            <TableCell className="text-right tabular-nums">
              {mismatch.journal}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

// One account's verdict, a switch over the contract's three values, so a
// fourth becomes a type error rather than nothing drawn. The tick is
// `matched`'s alone.
function VerdictLine({
  status,
}: {
  status: TinvestReconcileStatus;
}): React.JSX.Element {
  const { t } = useTranslation();
  switch (status) {
    case "not_checked":
      return (
        <p className="flex items-center gap-2 text-sm font-medium">
          <CircleDashed className="size-4 text-muted-foreground" />
          {t("connections.detail.reconcile.notChecked")}
        </p>
      );
    case "matched":
      return (
        <p className="flex items-center gap-2 text-sm font-medium">
          <CheckCircle2 className="size-4 text-emerald-600" />
          {t("connections.detail.reconcile.matched")}
        </p>
      );
    case "mismatched":
      return (
        <p className="flex items-center gap-2 text-sm font-medium">
          <TriangleAlert className="size-4 text-destructive" />
          {t("connections.detail.reconcile.mismatched")}
        </p>
      );
  }
}

// One account's verdict: whose it is, what the check said, when it was made and
// — when something differed — what did.
function AccountVerdict({
  connectionId,
  reconcile,
  accountName,
}: {
  connectionId: string;
  reconcile: TinvestAccountReconcile;
  accountName: string | undefined;
}) {
  const { t } = useTranslation();

  // Null with no check to time or an unparseable instant: the row then says it
  // could not read the time. The status decides: `at` is null exactly when
  // `not_checked`, and a contradiction drops the time.
  const checkedAt = (() => {
    if (reconcile.status === "not_checked") return null;
    if (reconcile.at === null) return null;
    const at = formatDateTime(reconcile.at);
    return at
      ? t("connections.detail.reconcile.checkedAt", { time: at })
      : t("connections.detail.reconcile.checkedAtUnreadable");
  })();

  return (
    <li className="grid gap-2 rounded-lg border p-3">
      <div className="grid gap-0.5">
        {/* Both ends of the pair: the babki account and the broker's label
           from when the link was made. */}
        <Link
          to="/accounts/$accountId"
          params={{ accountId: reconcile.account_id }}
          className="text-sm font-medium text-primary underline underline-offset-4"
        >
          {accountName ?? t("connections.detail.accountFallback")}
        </Link>
        <span
          className="text-xs text-muted-foreground"
          title={t("connections.detail.brokerAccountNameFrozen")}
        >
          {t("connections.detail.brokerAccount", {
            name: reconcile.broker_account_name,
          })}
        </span>
      </div>

      <VerdictLine status={reconcile.status} />
      {checkedAt && (
        <p className="text-xs text-muted-foreground">{checkedAt}</p>
      )}

      {/* Rendered off the list, not the status, which the server derives
         from it. */}
      {reconcile.mismatches.length > 0 && (
        <MismatchTable
          connectionId={connectionId}
          mismatches={reconcile.mismatches}
        />
      )}

      {/* A cash difference that cannot close says so: only with both
         unimported currency trades and a money difference. Never on
         securities rows: a currency trade moves no shares. */}
      {reconcile.currency_trades_unparsed > 0 &&
        reconcile.mismatches.some((m) => m.kind === "currency") && (
          <p
            data-testid="reconcile-currency-trades-note"
            className="text-xs text-muted-foreground"
          >
            {t("connections.detail.reconcile.currencyTradesExplainCash", {
              count: reconcile.currency_trades_unparsed,
            })}
          </p>
        )}
    </li>
  );
}

// The connection's line is derived (overallOf), never borrowed from the last
// account checked; «Сходится» only when every account agreed. A switch, so a new
// state cannot be forgotten.
function OverallVerdict({
  overall,
  someUnchecked,
}: {
  overall: Overall;
  someUnchecked: boolean;
}): React.JSX.Element {
  const { t } = useTranslation();
  switch (overall) {
    case "none":
      return (
        <p className="text-sm text-muted-foreground">
          {t("connections.detail.reconcile.overall.noAccounts")}
        </p>
      );
    case "not_checked":
      return (
        <div className="grid gap-1">
          <p className="flex items-center gap-2 font-medium">
            <CircleDashed className="size-4 text-muted-foreground" />
            {t("connections.detail.reconcile.overall.notChecked")}
          </p>
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.reconcile.overall.notCheckedBody")}
          </p>
        </div>
      );
    case "partial":
      return (
        <div className="grid gap-1">
          <p className="flex items-center gap-2 font-medium">
            <CircleDashed className="size-4 text-muted-foreground" />
            {t("connections.detail.reconcile.overall.partial")}
          </p>
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.reconcile.overall.partialBody")}
          </p>
        </div>
      );
    case "matched":
      return (
        <div className="grid gap-1">
          <p className="flex items-center gap-2 font-medium">
            <CheckCircle2 className="size-4 text-emerald-600" />
            {t("connections.detail.reconcile.overall.matched")}
          </p>
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.reconcile.overall.matchedBody")}
          </p>
        </div>
      );
    case "mismatched":
      return (
        <div className="grid gap-1">
          <p className="flex items-center gap-2 font-medium">
            <TriangleAlert className="size-4 text-destructive" />
            {t("connections.detail.reconcile.overall.mismatched")}
          </p>
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.reconcile.overall.mismatchedBody")}
          </p>
          {/* A difference found somewhere is not a report on everywhere. */}
          {someUnchecked && (
            <p className="text-sm text-muted-foreground">
              {t("connections.detail.reconcile.overall.someNotChecked")}
            </p>
          )}
        </div>
      );
  }
}

// ReconcilePanel draws the last check per account, the connection-wide
// conclusion, and how many broker operations remain unreadable.
export function ReconcilePanel({
  connectionId,
  reconciles,
}: {
  connectionId: string;
  reconciles: TinvestAccountReconcile[];
}) {
  const { t } = useTranslation();
  const overall = overallOf(reconciles);
  const someUnchecked = reconciles.some((r) => r.status === "not_checked");
  const accounts = useAccounts();
  const accountName = (accountId: string) =>
    accounts.data?.find((account) => account.id === accountId)?.name;

  // The same query and key as the list below, so the counter and the rows are
  // one answer, not two computations.
  const unparsed = useUnparsed(connectionId);
  // Explained rows are on the list (the only place to see or undo them) but not
  // counted: they are not unreadable. Told apart by explained_by, not by a
  // missing reason, which a row being rebuilt also lacks.
  const loadedUnparsed =
    unparsed.data?.pages.reduce(
      (rows, page) =>
        rows + page.operations.filter((o) => !o.explained_by).length,
      0,
    ) ?? 0;

  // Only what was fetched: with has_more the count is a floor, not a total.
  const unparsedCaption = (() => {
    if (unparsed.isPending) return null;
    if (unparsed.isError)
      return t("connections.detail.reconcile.unparsedUnknown");
    if (unparsed.hasNextPage)
      return t("connections.detail.reconcile.unparsedAtLeast", {
        n: loadedUnparsed,
      });
    if (loadedUnparsed === 0)
      return t("connections.detail.reconcile.unparsedNone");
    return t("connections.detail.reconcile.unparsedCount", {
      n: loadedUnparsed,
    });
  })();

  const hasInstrumentRow = reconciles.some((r) =>
    r.mismatches.some((m) => m.kind === "instrument"),
  );
  const hasUnsupportedRow = reconciles.some((r) =>
    r.mismatches.some((m) => m.kind === "unsupported"),
  );
  // A paper with no operations in the journal gets its own sentence, not
  // the unparsed one.
  const hasUnknownSecurityRow = reconciles.some((r) =>
    r.mismatches.some((m) => m.kind === "unknown_security"),
  );

  // «Они перечислены ниже» is said only when the counter counted something:
  // not while loading, not on error, not at zero.
  const unparsedIsKnownNonEmpty =
    !unparsed.isPending && !unparsed.isError && loadedUnparsed > 0;

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("connections.detail.reconcile.title")}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        <OverallVerdict overall={overall} someUnchecked={someUnchecked} />

        {reconciles.length > 0 && (
          <ul className="grid gap-3">
            {reconciles.map((reconcile) => (
              <AccountVerdict
                key={reconcile.link_id}
                connectionId={connectionId}
                reconcile={reconcile}
                accountName={accountName(reconcile.account_id)}
              />
            ))}
          </ul>
        )}

        {hasInstrumentRow && unparsedIsKnownNonEmpty && (
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.reconcile.instrumentNote")}
          </p>
        )}
        {hasUnknownSecurityRow && (
          <p
            className="text-sm text-muted-foreground"
            data-testid="reconcile-unknown-security-note"
          >
            {t("connections.detail.reconcile.unknownSecurityNote")}
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
