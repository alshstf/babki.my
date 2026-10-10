import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { usePayouts, type Payout } from "@/api/payouts";
import { PayoutsReceived } from "./received";
import { useAccounts } from "@/api/accounts";
import { useInstrumentIndex } from "@/api/instruments";
import { QueryGate, RefreshFailedNotice } from "@/components/query-notice";
import { queryState, refreshFailed } from "@/lib/query-state";
import { formatMinor, formatMinorCompact } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { cn } from "@/lib/utils";

const HORIZONS = [3, 6, 12, 24] as const;
const EVERY = "all";

// «Октябрь 2026».
function monthTitle(m: string): string {
  const name = new Date(`${m}-01T00:00:00`).toLocaleDateString("ru-RU", { month: "long", year: "numeric" });
  return name.charAt(0).toUpperCase() + name.slice(1).replace(" г.", "");
}

// PayoutsPage is what the papers the family holds today will pay: coupons,
// repayments, redemptions and declared dividends, month by month, before tax.
export function PayoutsPage() {
  const { t } = useTranslation();
  const [months, setMonths] = useState<number>(12);
  const [accountId, setAccountId] = useState(EVERY);
  const forecast = usePayouts(months, accountId === EVERY ? undefined : accountId);
  const accounts = useAccounts();
  const instruments = useInstrumentIndex();
  const state = queryState(forecast);
  const data = forecast.data;
  const brokers = (accounts.data ?? []).filter((a) => a.type === "brokerage" && a.status === "active");
  const accountName = (id: string) => accounts.data?.find((a) => a.id === id)?.name ?? "—";
  const paperName = (id: string) => instruments.get(id)?.name ?? `#${id.slice(-8)}`;

  const byMonth = new Map<string, Payout[]>();
  for (const p of data?.payouts ?? []) {
    const key = p.on.slice(0, 7);
    byMonth.set(key, [...(byMonth.get(key) ?? []), p]);
  }
  const peak = Math.max(1, ...(data?.by_month ?? [0]));

  return (
    <div className="grid gap-6">
      <div className="grid gap-1">
        <h1 className="text-2xl font-bold">{t("payouts.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("payouts.hint")}</p>
      </div>
      <div className="flex flex-wrap gap-2">
        <Select value={String(months)} onValueChange={(v) => setMonths(Number(v))}>
          <SelectTrigger className="w-48" aria-label={t("payouts.horizon")}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {HORIZONS.map((h) => (
              <SelectItem key={h} value={String(h)}>
                {t("payouts.months", { n: h })}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={accountId} onValueChange={setAccountId}>
          <SelectTrigger className="w-56" aria-label={t("payouts.account")}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={EVERY}>{t("payouts.allAccounts")}</SelectItem>
            {brokers.map((a) => (
              <SelectItem key={a.id} value={a.id}>
                {a.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <RefreshFailedNotice show={refreshFailed(forecast)} />
      <QueryGate state={state} />
      {state === "ready" && data && (
        <>
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-3">
            <Card data-testid="payouts-total">
              <CardHeader className="pb-1">
                <CardTitle className="text-sm font-normal text-muted-foreground">
                  {t("payouts.totalFor", { n: months })}
                </CardTitle>
              </CardHeader>
              <CardContent className="text-xl font-semibold tabular-nums sm:text-2xl">
                {formatMinorCompact(data.total_minor, data.base_currency)}
              </CardContent>
            </Card>
            <Card data-testid="payouts-this-month">
              <CardHeader className="pb-1">
                <CardTitle className="text-sm font-normal text-muted-foreground">{t("payouts.thisMonth")}</CardTitle>
              </CardHeader>
              <CardContent className="text-xl font-semibold tabular-nums sm:text-2xl">
                {formatMinorCompact(data.by_month[0] ?? 0, data.base_currency)}
              </CardContent>
            </Card>
          </div>
          {data.missing_rates.length > 0 && (
            <Alert>
              <AlertDescription>
                {t("payouts.missingRates", { currencies: data.missing_rates.join(", ") })}
              </AlertDescription>
            </Alert>
          )}
          {/* A bar per month, so the lean ones show at a glance. */}
          <div className="grid gap-1" data-testid="payouts-months">
            {data.months.map((m, i) => (
              <div key={m} className="flex items-center gap-3 text-sm">
                <span className="w-32 shrink-0 text-muted-foreground">{monthTitle(m)}</span>
                <div className="h-2 flex-1 overflow-hidden rounded bg-muted">
                  <div className="h-full rounded bg-emerald-500" style={{ width: `${(data.by_month[i] / peak) * 100}%` }} />
                </div>
                <span className="w-28 shrink-0 text-right tabular-nums">
                  {data.by_month[i] ? formatMinorCompact(data.by_month[i], data.base_currency) : "—"}
                </span>
              </div>
            ))}
          </div>
          {data.payouts.length === 0 ? (
            <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
              {t("payouts.empty")}
            </div>
          ) : (
            data.months
              .filter((m) => byMonth.has(m))
              .map((m) => (
                <Card key={m} data-testid="payouts-month">
                  <CardHeader className="pb-2">
                    <CardTitle className="text-base">{monthTitle(m)}</CardTitle>
                  </CardHeader>
                  <CardContent className="overflow-x-auto">
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>{t("payouts.columns.date")}</TableHead>
                          <TableHead>{t("payouts.columns.paper")}</TableHead>
                          <TableHead className="hidden sm:table-cell">{t("payouts.columns.account")}</TableHead>
                          <TableHead className="text-right">{t("payouts.columns.amount")}</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {(byMonth.get(m) ?? []).map((p, i) => (
                          <TableRow key={`${p.instrument_id}-${p.account_id}-${p.kind}-${p.on}-${i}`} data-testid="payout-row">
                            <TableCell className="whitespace-nowrap">{formatDate(p.on)}</TableCell>
                            <TableCell className="whitespace-normal">
                              <Link
                                to="/instruments/$instrumentId"
                                params={{ instrumentId: p.instrument_id }}
                                className="hover:underline"
                              >
                                {paperName(p.instrument_id)}
                              </Link>
                              <div className="flex flex-wrap items-center gap-1 text-xs text-muted-foreground">
                                <Badge variant={p.kind === "offer" ? "outline" : "secondary"}>
                                  {t(`payouts.kinds.${p.kind}`)}
                                </Badge>
                                <span>
                                  {p.quantity} ×{" "}
                                  {p.per_unit != null
                                    ? t("payouts.perUnit", { amount: p.per_unit, currency: p.currency })
                                    : "…"}
                                </span>
                              </div>
                            </TableCell>
                            <TableCell className="hidden sm:table-cell">{accountName(p.account_id)}</TableCell>
                            <TableCell className={cn("text-right tabular-nums", p.amount_minor == null && "text-muted-foreground")}>
                              {p.amount_minor != null ? (
                                <>
                                  {formatMinor(p.amount_minor, p.currency)}
                                  {p.currency !== data.base_currency && p.in_base_minor != null && (
                                    <div className="text-xs text-muted-foreground">
                                      ≈ {formatMinor(p.in_base_minor, data.base_currency)}
                                    </div>
                                  )}
                                </>
                              ) : p.kind === "offer" ? (
                                t("payouts.offerNote")
                              ) : (
                                t("payouts.notSet")
                              )}
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </CardContent>
                </Card>
              ))
          )}
        </>
      )}
      <PayoutsReceived
        accountId={accountId === EVERY ? undefined : accountId}
        paperName={paperName}
        accountName={accountName}
      />
    </div>
  );
}
