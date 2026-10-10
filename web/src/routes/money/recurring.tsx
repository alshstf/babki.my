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
import { useState } from "react";
import { EyeOff } from "lucide-react";
import { Button } from "@/components/ui/button";
import { keyOf, useHideRecurring, useRecurring } from "@/api/recurring";
import { useSession } from "@/api/session";
import { useAccounts } from "@/api/accounts";
import { formatMinor, signClass } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { cn } from "@/lib/utils";
import { useNarrow } from "@/lib/use-narrow";

// RecurringPayments lists the payments that come back — rent, the phone, a
// subscription, a salary — with when each is due next. Nothing at all while
// the journals show none.
export function RecurringPayments() {
  const { t } = useTranslation();
  const narrow = useNarrow();
  const recurring = useRecurring();
  const accounts = useAccounts();
  const { data: session } = useSession();
  const hide = useHideRecurring();
  const [showHidden, setShowHidden] = useState(false);
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const all = recurring.data ?? [];
  const list = all.filter((p) => !p.hidden);
  const hidden = all.filter((p) => p.hidden);
  if (all.length === 0) return null;
  const accountName = (id: string) => accounts.data?.find((a) => a.id === id)?.name ?? "—";
  // When the payment is due: a late one in amber, said as expected.
  const nextText = (overdue: boolean, on: string) =>
    overdue ? t("recurring.expected", { date: formatDate(on) }) : formatDate(on);
  const nextClass = (overdue: boolean) => cn(overdue && "text-amber-700 dark:text-amber-400");
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
              {!narrow && <TableHead>{t("recurring.columns.next")}</TableHead>}
              <TableHead className="text-right">{t("recurring.columns.amount")}</TableHead>
              {canEdit && <TableHead className="w-10" />}
            </TableRow>
          </TableHeader>
          <TableBody>
            {list.map((p) => (
              <TableRow key={`${p.name}-${p.currency}-${p.amount_minor > 0}-${p.next_on}`} data-testid="recurring-row">
                <TableCell className="whitespace-normal">
                  {p.name}
                  <div>
                    <Badge variant="outline">{t(`recurring.cadences.${p.cadence}`)}</Badge>
                  </div>
                  {/* On a phone the next date folds under the name. */}
                  {narrow && <div className={nextClass(p.overdue)}>{nextText(p.overdue, p.next_on)}</div>}
                </TableCell>
                <TableCell className="hidden sm:table-cell">{accountName(p.account_id)}</TableCell>
                {!narrow && (
                  <TableCell className={cn("whitespace-nowrap", nextClass(p.overdue))}>{nextText(p.overdue, p.next_on)}</TableCell>
                )}
                <TableCell className={cn("text-right tabular-nums", signClass(p.amount_minor))}>
                  {formatMinor(p.amount_minor, p.currency)}
                </TableCell>
                {canEdit && (
                  <TableCell>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={t("recurring.hide")}
                      title={t("recurring.hide")}
                      disabled={hide.isPending}
                      onClick={() => hide.mutate({ key: keyOf(p), hide: true })}
                    >
                      <EyeOff className="size-4" />
                    </Button>
                  </TableCell>
                )}
              </TableRow>
            ))}
          </TableBody>
        </Table>
        {hidden.length > 0 && (
          <div className="mt-3 grid gap-1 text-sm" data-testid="recurring-hidden">
            <Button variant="link" size="sm" className="justify-self-start p-0" onClick={() => setShowHidden(!showHidden)}>
              {t("recurring.hiddenCount", { count: hidden.length })}
            </Button>
            {showHidden &&
              hidden.map((p) => (
                <div key={`${p.name}-${p.currency}-${p.amount_minor > 0}-${p.next_on}`} className="flex flex-wrap items-center gap-2 text-muted-foreground">
                  <span>
                    {p.name} · {formatMinor(p.amount_minor, p.currency)}
                  </span>
                  {canEdit && (
                    <Button variant="ghost" size="sm" disabled={hide.isPending} onClick={() => hide.mutate({ key: keyOf(p), hide: false })}>
                      {t("recurring.unhide")}
                    </Button>
                  )}
                </div>
              ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
