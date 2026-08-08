import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
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
import { useAccounts } from "@/api/accounts";
import {
  useUnparsed,
  type TinvestAccountReconcile,
  type TinvestReconcileMismatch,
  type TinvestReconcileStatus,
} from "@/api/connections";

// A VERDICT BELONGS TO THE ACCOUNT IT WAS MADE FOR. A sync run is made for one
// broker account and the check against the broker happens inside it, so what it
// found says nothing about the connection's other accounts. The server publishes
// one verdict per linked account for exactly that reason (see
// TinvestAccountReconcile), and this panel draws them one by one.
//
// «Не проверено» IS NOT «СХОДИТСЯ» AND NOT «РАСХОЖДЕНИЙ НЕТ». An account no run
// ever reconciled makes no claim about the broker whatsoever, and the tick
// belongs to `matched` alone — this project has drawn a caption over a figure
// that did not support it four times, and this is the caption most able to do
// it again, because an empty mismatch list looks exactly like agreement.

// What the card says about the CONNECTION, derived from the accounts and never
// guessed from one of them:
//
//   - "matched" needs every account checked and every one of them agreeing.
//     Anything less is one of the three below.
//   - "mismatched": at least one account differs. True whether or not the rest
//     were checked; when some were not, the block says so in its own sentence
//     rather than letting a reader take what was found for all there is.
//   - "partial": nothing differs where anyone looked, and somewhere nobody did.
//   - "not_checked": nobody looked anywhere.
//   - "none": the connection has no accounts left to check (deleting a babki
//     account takes its link with it and leaves the connection standing).
type Overall = "none" | "not_checked" | "partial" | "matched" | "mismatched";

function overallOf(list: TinvestAccountReconcile[]): Overall {
  if (list.length === 0) return "none";
  if (list.some((r) => r.status === "mismatched")) return "mismatched";
  if (list.every((r) => r.status === "matched")) return "matched";
  if (list.every((r) => r.status === "not_checked")) return "not_checked";
  return "partial";
}

// One account's differences, printed as they arrived. Both figures, never their
// difference: a reader who sees only the gap cannot tell which side to look at.
function MismatchTable({ mismatches }: { mismatches: TinvestReconcileMismatch[] }) {
  const { t } = useTranslation();
  return (
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
          // The rows carry no id of their own — a currency row and an unmatched
          // broker position both have a null instrument_id — and the list is a
          // snapshot that is never reordered or edited in place, so the position
          // in it is the only key there is.
          <TableRow key={`${mismatch.kind}-${mismatch.label}-${index}`}>
            <TableCell>{mismatch.label}</TableCell>
            <TableCell>
              {/* Three different pieces of news, and the contract keeps them
                  apart precisely so a reader is not sent hunting for missing
                  operations over an asset this program was never going to
                  hold. */}
              <Badge variant="secondary">
                {t(`connections.mismatchKinds.${mismatch.kind}`)}
              </Badge>
            </TableCell>
            <TableCell className="text-right tabular-nums">{mismatch.broker}</TableCell>
            <TableCell className="text-right tabular-nums">{mismatch.journal}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

// One account's verdict in one line. A SWITCH OVER THE CONTRACT'S OWN THREE
// VALUES rather than three separate tests, the way runs-table.tsx switches over
// a run's status and for the same reason: a fourth value added to the contract
// arrives here as a type error instead of drawing nothing at all, silently, on
// the card whose whole job is to say what the check found. The tick is
// `matched`'s alone.
function VerdictLine({ status }: { status: TinvestReconcileStatus }): React.JSX.Element {
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
  reconcile,
  accountName,
}: {
  reconcile: TinvestAccountReconcile;
  accountName: string | undefined;
}) {
  const { t } = useTranslation();

  // Null when there is no check to time, and when the instant will not parse:
  // half a sentence ending in nothing claims less than nothing, so the row says
  // that it could not read the time rather than printing «Проверено ».
  //
  // THE STATUS DECIDES, NOT THE TIMESTAMP. The contract says `at` is null
  // exactly when the status is `not_checked`; if the pair ever arrived
  // contradicting itself, «Проверено 04.08.2026» printed under «Не проверено»
  // would be the screen saying both, so the verdict wins and the time is
  // dropped.
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
        {/* Both ends of the pair, named the way the connection's own list of
            accounts names them: the babki account this import feeds, and the
            broker's label frozen at the moment the link was made. */}
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
          {t("connections.detail.brokerAccount", { name: reconcile.broker_account_name })}
        </span>
      </div>

      <VerdictLine status={reconcile.status} />
      {checkedAt && <p className="text-xs text-muted-foreground">{checkedAt}</p>}

      {/* Rendered off the LIST rather than off the status, so the table and the
          word above it cannot come apart: the contract derives one from the
          other on the server (empty exactly when `matched`), and reading the
          list here keeps that single fact single. */}
      {reconcile.mismatches.length > 0 && (
        <MismatchTable mismatches={reconcile.mismatches} />
      )}
    </li>
  );
}

// THE CONNECTION'S OWN LINE IS DERIVED, NEVER BORROWED from whichever account
// was checked last. «Сходится» is said only where overallOf could establish it —
// every account checked, every one agreeing — and each of the other four states
// names what is missing instead. A switch again, so a state added to Overall
// cannot be forgotten here and leave the card with no conclusion at all.
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

// ReconcilePanel draws the last check against the broker FOR EACH of the
// connection's accounts, the connection-wide conclusion derived from all of
// them, and — beside it — how many of the broker's operations this program
// still could not read.
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

  const hasInstrumentRow = reconciles.some((r) =>
    r.mismatches.some((m) => m.kind === "instrument"),
  );
  const hasUnsupportedRow = reconciles.some((r) =>
    r.mismatches.some((m) => m.kind === "unsupported"),
  );

  // «Они перечислены ниже» IS A CLAIM ABOUT THE LIST, so it is made only where
  // the list backs it. A security difference does NOT require a single
  // unreadable operation — a plain difference in quantities is one, and so is a
  // position the broker stopped returning — and the sentence used to be printed
  // straight above «Неразобранных операций нет». It is shown now when the
  // counter beside it has actually counted something: never while the list is
  // still loading, never when it could not be read, and never at zero.
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
