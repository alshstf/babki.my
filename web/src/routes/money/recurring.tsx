import { useTranslation } from "react-i18next";
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
import { useRecurring } from "@/api/recurring";
import { useAccounts } from "@/api/accounts";
import { formatMinor, signClass } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { cn } from "@/lib/utils";

// RecurringPayments lists the payments that come back — rent, the phone, a
// subscription, a salary — with when each is due next. Nothing at all while
// the journals show none.
export function RecurringPayments() {
  const { t } = useTranslation();
  const recurring = useRecurring();
  const accounts = useAccounts();
  const list = recurring.data ?? [];
  if (list.length === 0) return null;
  const accountName = (id: string) => accounts.data?.find((a) => a.id === id)?.name ?? "—";
  return (
    <Card data-testid="money-recurring">
      <CardHeader>
        <CardTitle>{t("recurring.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("recurring.hint")}</p>
      </CardHeader>
      <CardContent className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("recurring.columns.name")}</TableHead>
              <TableHead className="hidden sm:table-cell">{t("recurring.columns.account")}</TableHead>
              <TableHead>{t("recurring.columns.next")}</TableHead>
              <TableHead className="text-right">{t("recurring.columns.amount")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {list.map((p) => (
              <TableRow key={`${p.name}-${p.currency}-${p.amount_minor > 0}`} data-testid="recurring-row">
                <TableCell className="whitespace-normal">
                  {p.name}
                  <div>
                    <Badge variant="outline">{t(`recurring.cadences.${p.cadence}`)}</Badge>
                  </div>
                </TableCell>
                <TableCell className="hidden sm:table-cell">{accountName(p.account_id)}</TableCell>
                <TableCell className={cn("whitespace-nowrap", p.overdue && "text-amber-700 dark:text-amber-400")}>
                  {p.overdue ? t("recurring.expected", { date: formatDate(p.next_on) }) : formatDate(p.next_on)}
                </TableCell>
                <TableCell className={cn("text-right tabular-nums", signClass(p.amount_minor))}>
                  {formatMinor(p.amount_minor, p.currency)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
