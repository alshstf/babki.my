import { Link } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useNarrow } from "@/lib/use-narrow";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useInstrumentOperations } from "@/api/instrument-page";
import { formatMinor, formatPriceIn, signClass } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { cn } from "@/lib/utils";

// Every row of the paper across the family's accounts, newest first: what was
// bought, sold, paid out or moved, and on which account. Correcting a row is
// done in its account's journal, where the account's own checks apply.
export function PaperOperations({
  instrumentId,
  accountName,
}: {
  instrumentId: string;
  accountName: (id: string) => string | undefined;
}) {
  const { t } = useTranslation();
  const narrow = useNarrow();
  const pages = useInstrumentOperations(instrumentId);
  const rows = (pages.data?.pages ?? []).flatMap((p) => p.operations);
  if (rows.length === 0) return null;
  return (
    <div className="grid gap-2">
      <Table data-testid="paper-operations">
        <TableHeader>
          <TableRow>
            <TableHead>{t("operations.columns.date")}</TableHead>
            {narrow ? (
              <TableHead>{t("operations.columns.type")}</TableHead>
            ) : (
              <>
                <TableHead>{t("positions.columns.account")}</TableHead>
                <TableHead>{t("operations.columns.type")}</TableHead>
                <TableHead className="text-right">{t("positions.columns.quantity")}</TableHead>
                <TableHead className="text-right">{t("instrumentPage.columns.price")}</TableHead>
              </>
            )}
            <TableHead className="text-right">{t("instrumentPage.columns.amount")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((op) => (
            <TableRow key={op.id}>
              <TableCell className="whitespace-nowrap">{formatDate(op.occurred_on)}</TableCell>
              {narrow ? (
                // On a phone the account, the count and the price fold under the kind.
                <TableCell className="whitespace-normal">
                  {t(`operationTypes.${op.type}`)}
                  <div className="text-xs text-muted-foreground">
                    <Link to="/accounts/$accountId" params={{ accountId: op.account_id }} className="hover:underline">
                      {accountName(op.account_id) ?? t("instrumentPage.unknownAccount")}
                    </Link>
                  </div>
                  {op.quantity && op.price && (
                    <div className="text-xs text-muted-foreground tabular-nums">
                      {op.quantity} × {formatPriceIn(op.price, op.currency) ?? op.price}
                    </div>
                  )}
                </TableCell>
              ) : (
                <>
                  <TableCell>
                    <Link to="/accounts/$accountId" params={{ accountId: op.account_id }} className="hover:underline">
                      {accountName(op.account_id) ?? t("instrumentPage.unknownAccount")}
                    </Link>
                  </TableCell>
                  <TableCell>{t(`operationTypes.${op.type}`)}</TableCell>
                  <TableCell className="text-right tabular-nums">{op.quantity ?? ""}</TableCell>
                  <TableCell className="text-right tabular-nums">
                    {op.price ? (formatPriceIn(op.price, op.currency) ?? op.price) : ""}
                  </TableCell>
                </>
              )}
              <TableCell className={cn("text-right tabular-nums", signClass(op.amount_minor))}>
                {formatMinor(op.amount_minor, op.currency)}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {pages.hasNextPage && (
        <Button
          variant="outline"
          size="sm"
          className="justify-self-start"
          disabled={pages.isFetchingNextPage}
          onClick={() => void pages.fetchNextPage()}
        >
          {t("instrumentPage.more")}
        </Button>
      )}
    </div>
  );
}
