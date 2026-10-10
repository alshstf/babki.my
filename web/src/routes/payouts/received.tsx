import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { usePayoutsReceived, type PayoutCheck } from "@/api/payouts";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { cn } from "@/lib/utils";

const DAYS = 90;

const tone: Record<PayoutCheck["status"], string> = {
  received: "text-emerald-700 dark:text-emerald-400",
  short: "text-amber-700 dark:text-amber-400",
  missing: "text-red-700 dark:text-red-400",
  awaited: "text-muted-foreground",
};

// PayoutsReceived checks the payouts due in the last three months against
// the journals (#424): came, came short, missing, awaited — a broker's missed
// coupon shows here. Nothing while none was due.
export function PayoutsReceived({
  accountId,
  paperName,
  accountName,
}: {
  accountId?: string;
  paperName: (id: string) => string;
  accountName: (id: string) => string;
}) {
  const { t } = useTranslation();
  const received = usePayoutsReceived(DAYS, accountId);
  const checks = received.data ?? [];
  if (checks.length === 0) return null;
  const count = (s: PayoutCheck["status"]) => checks.filter((c) => c.status === s).length;
  const status = (s: PayoutCheck["status"]) =>
    s === "received" ? t("payouts.received.statuses.received")
      : s === "short" ? t("payouts.received.statuses.short")
        : s === "missing" ? t("payouts.received.statuses.missing")
          : t("payouts.received.statuses.awaited");
  return (
    <Card data-testid="payouts-received">
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{t("payouts.received.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("payouts.received.hint")}</p>
        <p className="text-sm" data-testid="payouts-received-summary">
          {t("payouts.received.summary", {
            received: count("received"),
            short: count("short"),
            missing: count("missing"),
            awaited: count("awaited"),
          })}
        </p>
      </CardHeader>
      <CardContent className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("payouts.columns.date")}</TableHead>
              <TableHead>{t("payouts.columns.paper")}</TableHead>
              <TableHead className="hidden sm:table-cell">{t("payouts.columns.account")}</TableHead>
              <TableHead className="text-right">{t("payouts.received.due")}</TableHead>
              <TableHead className="text-right">{t("payouts.received.came")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {checks.map((c, i) => (
              <TableRow key={`${c.instrument_id}-${c.account_id}-${c.kind}-${c.on}-${i}`} data-testid="payout-check">
                <TableCell className="whitespace-nowrap">{formatDate(c.on)}</TableCell>
                <TableCell className="whitespace-normal">
                  <Link to="/instruments/$instrumentId" params={{ instrumentId: c.instrument_id }} className="hover:underline">
                    {paperName(c.instrument_id)}
                  </Link>
                  <div className="text-xs text-muted-foreground">
                    <Badge variant="secondary">{t(`payouts.kinds.${c.kind}`)}</Badge> {c.quantity} ×{" "}
                    {c.per_unit != null ? t("payouts.perUnit", { amount: c.per_unit, currency: c.currency }) : "…"}
                  </div>
                </TableCell>
                <TableCell className="hidden sm:table-cell">{accountName(c.account_id)}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {c.amount_minor != null ? formatMinor(c.amount_minor, c.currency) : "…"}
                </TableCell>
                <TableCell className={cn("text-right", tone[c.status])} data-testid="payout-check-status">
                  <div className="font-medium">{status(c.status)}</div>
                  {c.got_on != null && c.got_currency != null && (
                    <div className="text-xs tabular-nums">
                      {formatMinor(c.got_minor, c.got_currency)} · {formatDate(c.got_on)}
                    </div>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
