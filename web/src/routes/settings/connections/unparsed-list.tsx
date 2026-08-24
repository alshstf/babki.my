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

// UnparsedList shows the broker's operations that this program could not turn
// into journal entries: what the broker said happened, why the projection
// refused it, and — folded away — the broker's own JSON for the person who
// wants to see exactly what arrived.
//
// This list is the honest half of «честность вместо тишины»: every one of these
// rows is an operation the positions and the profit above do NOT account for,
// and the alternative to showing them is a screen that silently understates
// what the owner holds.
export function UnparsedList({ connectionId }: { connectionId: string }) {
  const { t } = useTranslation();
  // The rows picked, as (link, content key) pairs. A connection may feed
  // SEVERAL broker accounts and this list covers all of them, while one
  // journal operation belongs to one account — so a selection may never span
  // two links, and the link of the first pick is what the rest are measured
  // against (see toggle).
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
    // Picking a row of another broker account starts a new selection rather
    // than joining the two: one operation cannot stand for events on two
    // accounts, and silently dropping the earlier picks would be worse than
    // replacing them visibly.
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
        {/* «Брокер ИХ отдал, программа сохранила ИХ как есть» — a sentence about
            the rows, printed only where there are rows for it to be about. Over
            an empty list it introduced nothing, and «Неразобранных операций
            нет» directly underneath answered it. */}
        {/* Each sentence is printed only over rows it is TRUE of. The first
            one — «ни позиции, ни прибыль их не учитывают» — is false about an
            explained row, whose manual operation is counted in both; the second
            says so, and appears only when such a row is on the page. */}
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
        {/* «Неразобранных операций нет» and not «все операции разобраны»: this
            list is empty on a connection that has never synced anything at all,
            where there is nothing to have been read successfully either. The
            wording is true of both. */}
        {!unparsed.isPending && !unparsed.isError && list.length === 0 && (
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.unparsed.empty")}
          </p>
        )}
        {/* The explaining controls, offered only when there is something to
            explain and an account to write to. The count is on the button
            because the operation entered next will stand for exactly these
            rows, and a person about to enter one number for two events should
            see how many they picked. */}
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
                    {/* An explained row cannot be picked: it already has an
                        answer, and the way to change that answer is to take the
                        old one back first — which is what the «Снять» button
                        beside its caption does. */}
                    {operation.explained_by == null && (
                      <Checkbox
                        aria-label={t("connections.detail.explain.pick")}
                        checked={picked.includes(operation.content_key)}
                        onCheckedChange={() => toggle(operation.link_id, operation.content_key)}
                      />
                    )}
                  </TableCell>
                  <TableCell className="whitespace-nowrap">
                    {formatDateTime(operation.occurred_at)}
                  </TableCell>
                  <TableCell>
                    {/* The broker's own word for the type, verbatim. Nothing
                        here translates it, because a row is on this list
                        exactly when this program had no rule for what the
                        broker sent — inventing a Russian name for it would be
                        claiming an understanding that is missing. */}
                    <div className="grid gap-0.5">
                      <code className="text-xs">{operation.op_type}</code>
                      {/* WHERE THE BROKER SAYS IT HAPPENED, beside what it
                          says happened. The code is shown as it arrived and a
                          word is added only where this program has a source
                          for one — an unnamed code stands on its own rather
                          than being dressed in a guess, which is the same rule
                          the type above follows. Empty for an operation that
                          describes no instrument (money in and out), and then
                          there is nothing to show. */}
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
                  {/* The broker's own amount, exactly as it arrived: a sum too
                      fine or too large for this program's money is one of the
                      reasons a row lands here at all, so running it through the
                      money formatter would round away the very thing that made
                      it unreadable — or fail on it. */}
                  <TableCell className="text-right tabular-nums whitespace-nowrap">
                    {operation.payment} {operation.currency}
                  </TableCell>
                  <TableCell>
                    <div className="grid gap-1">
                      {/* AN EXPLAINED ROW HAS NO REASON, and printing the empty
                          one would render the Russian name of a code that is
                          not there. What is true of it is the owner's own
                          answer, so that is what stands here — with the
                          operation it points at, and the way to take it back.
                          The distinction is read from `explained_by` and never
                          from an empty `reason`, which a row still being
                          rebuilt also has. */}
                      {operation.explained_by != null ? (
                        <>
                          <span>
                            {t("connections.detail.explain.explainedBy", {
                              type: t(`operationTypes.${operation.explained_by.operation_type}`),
                              date: formatDate(operation.explained_by.operation_on),
                            })}
                          </span>
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
                            {/* Said next to the button rather than in a
                                confirmation nobody reads: the journal entry
                                goes too, and that is money leaving the
                                account's figures. */}
                            <p className="text-xs text-muted-foreground">
                              {t("connections.detail.explain.removeHint")}
                            </p>
                          </div>
                        </>
                      ) : (
                        <span>{t(`connections.unparsedReasons.${operation.reason}`)}</span>
                      )}
                      {/* What refused this row said about THIS row, printed
                          under the Russian name of the code. The code is what
                          chooses the sentence above — this is only shown, never
                          read: nothing here branches on it, and no wording is
                          matched.

                          Untranslated, like the broker's own type word and the
                          document under «Что прислал брокер» two lines below.
                          The i18n rule is that what this program SAYS is
                          Russian and comes from ru.json; this is not something
                          it says, it is a piece of the record being shown, and
                          a Russian paraphrase of it would be this screen
                          claiming to have understood a refusal it only carries.

                          The rule that a .tsx may not render the server's own
                          words is about an ERROR's `message`, built from a
                          failed response's body for a log. This is a field of a
                          successful answer, declared in the contract
                          (TinvestUnparsedOperation.detail) and carried for
                          exactly this purpose: «Операцию отклонил движок
                          журнала» was the whole story for 134 of the owner's
                          rows, and none of them could be acted on.

                          Printed only when there is something to print. An
                          empty detail is an ordinary state — a row refused
                          before the server kept details — and an empty line
                          under the reason would suggest something is loading. */}
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
