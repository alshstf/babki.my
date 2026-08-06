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
import { formatDateTime } from "@/lib/dates";
import { useUnparsed } from "@/api/connections";

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
  // The same query the reconcile panel's counter reads, by the same key — one
  // request, and one answer that both places state the same way.
  const unparsed = useUnparsed(connectionId);
  const list = unparsed.data?.pages.flatMap((page) => page.operations) ?? [];

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
        {list.length > 0 && (
          <p className="text-sm text-muted-foreground">{t("connections.detail.unparsed.intro")}</p>
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
        {list.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
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
                      <span>{t(`connections.unparsedReasons.${operation.reason}`)}</span>
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
