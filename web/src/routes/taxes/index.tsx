import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useQueryClient } from "@tanstack/react-query";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useAccounts, useUpdateAccount } from "@/api/accounts";
import { useMembers } from "@/api/members";
import { useSession } from "@/api/session";
import { useTaxYear, useWageEntry, type TaxYear, type TaxYearPerson, type TaxYearWagePayment } from "@/api/tax-year";
import { QueryGate } from "@/components/query-notice";
import { queryState } from "@/lib/query-state";
import { formatMinor, minorToInput, parseToMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";

// TaxesPage is the family's income tax for a year, an estimate (decisions
// Р-41, Р-42): per person, the tax on the year's investment incomes, what the
// brokers withhold, what is left to pay by return — or to get back — and by
// when; under it, each account's part.
export function TaxesPage() {
  const { t } = useTranslation();
  const [thisYear] = useState(() => new Date().getFullYear());
  const [year, setYear] = useState(thisYear);
  const q = useTaxYear(year);
  const state = queryState(q);
  const data = q.data;
  return (
    <div className="grid gap-6">
      <div className="grid gap-1">
        <h1 className="text-2xl font-bold">{t("taxes.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("taxes.hint")}</p>
      </div>
      <Select value={String(year)} onValueChange={(v) => setYear(Number(v))}>
        <SelectTrigger className="w-40" aria-label={t("taxes.year")}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {[thisYear, thisYear - 1, thisYear - 2].map((y) => (
            <SelectItem key={y} value={String(y)}>
              {t("taxes.yearOption", { year: y })}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <QueryGate state={state} />
      {data && !data.supported && (
        <Alert>
          <AlertDescription>{t("taxes.notSupported")}</AlertDescription>
        </Alert>
      )}
      {data && data.supported && data.people.length === 0 && (
        <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">{t("taxes.empty", { year: data.year })}</div>
      )}
      {data && data.supported && data.people.map((p) => <PersonCard key={p.user_id} year={data} person={p} />)}
      {data && data.supported && data.by_balance > 0 && (
        <p className="text-xs text-amber-700 dark:text-amber-400" data-testid="tax-by-balance">{t("taxes.byBalance", { count: data.by_balance })}</p>
      )}
      {data && data.supported && (
        <div className="grid gap-1 text-xs text-muted-foreground">
          <p>{t("taxes.notYet")}</p>
          <p>{t("taxes.accrued")}</p>
        </div>
      )}
      {data && data.supported && <SharedAccounts />}
    </div>
  );
}

function PersonCard({ year, person: p }: { year: TaxYear; person: TaxYearPerson }) {
  const { t } = useTranslation();
  const c = year.currency;
  return (
    <Card data-testid="tax-person">
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{p.name}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3">
        <div className="text-lg font-semibold" data-testid="tax-verdict">
          {p.to_pay_minor > 0
            ? t("taxes.toPay", { amount: formatMinor(p.to_pay_minor, c) })
            : p.refund_minor > 0
              ? t("taxes.refund", { amount: formatMinor(p.refund_minor, c) })
              : t("taxes.nothingToPay")}
        </div>
        {p.to_pay_minor > 0 && (
          <p className="text-sm text-muted-foreground">{t("taxes.toPayWhen", { declare: formatDate(year.declare_by), pay: formatDate(year.pay_by) })}</p>
        )}
        {p.refund_minor > 0 && (
          <p className="text-sm text-muted-foreground">
            {p.to_pay_minor > 0 && `${t("taxes.refundToo", { amount: formatMinor(p.refund_minor, c) })} `}
            {t("taxes.refundWhy", { declare: formatDate(year.declare_by) })}
          </p>
        )}
        <p className="text-sm" data-testid="tax-summary">
          {t("taxes.summary", { tax: formatMinor(p.tax_minor, c), base: formatMinor(p.base_minor, c) })}
        </p>
        <p className="text-sm text-muted-foreground" data-testid="tax-agents">
          {[
            t("taxes.byAgents", { amount: formatMinor(p.by_agents_minor, c) }),
            p.withheld_minor > 0 && t("taxes.withheld", { amount: formatMinor(p.withheld_minor, c) }),
            p.credit_minor > 0 && t("taxes.credit", { amount: formatMinor(p.credit_minor, c) }),
          ]
            .filter(Boolean)
            .join(" · ")}
        </p>
        {p.wages_minor > 0 && (
          <div className="grid gap-1 text-sm">
            <p data-testid="tax-wages">
              {t("taxes.wages", {
                net: formatMinor(p.wages_net_minor, c),
                gross: formatMinor(p.wages_minor, c),
                how: p.wage_payments.every((w) => w.entered)
                  ? t("taxes.wagesHowEntered")
                  : p.wage_payments.some((w) => w.entered)
                    ? t("taxes.wagesHowMixed")
                    : t("taxes.wagesHowReckoned"),
                tax: formatMinor(p.wages_tax_minor, c),
                withheld: formatMinor(p.wages_withheld_minor, c),
              })}
            </p>
            {p.wages_to_pay_minor > 0 && (
              <p className="text-amber-700 dark:text-amber-400">{t("taxes.wagesToPay", { amount: formatMinor(p.wages_to_pay_minor, c) })}</p>
            )}
            {p.employers.length > 1 && (
              <ul className="text-xs text-muted-foreground">
                {p.employers.map((e) => (
                  <li key={e.name}>
                    {t("taxes.employer", { name: e.name || t("taxes.noEmployer"), net: formatMinor(e.net_minor, c), gross: formatMinor(e.gross_minor, c) })}
                  </li>
                ))}
              </ul>
            )}
            <WagePayments payments={p.wage_payments} currency={c} />
          </div>
        )}
        {(p.property_sold_minor > 0 || p.property_free > 0) && (
          <p className="text-sm" data-testid="tax-property">
            {p.property_sold_minor > 0 &&
              t("taxes.property", {
                sold: formatMinor(p.property_sold_minor, c),
                gain: `${p.property_gain_minor >= 0 ? "" : "−"}${formatMinor(Math.abs(p.property_gain_minor), c)}`,
                base: formatMinor(p.property_base_minor, c),
              })}
            {p.property_free > 0 && ` ${t("taxes.propertyFree", { count: p.property_free })}`}
          </p>
        )}
        {p.interest_minor > 0 && (
          <p className="text-sm" data-testid="tax-interest">
            {p.interest_free_minor === null || year.key_rate === null
              ? t("taxes.interestNoRate", { interest: formatMinor(p.interest_minor, c) })
              : t("taxes.interest", {
                  interest: formatMinor(p.interest_minor, c),
                  free: formatMinor(p.interest_free_minor, c),
                  rate: Number(year.key_rate).toLocaleString("ru-RU", { maximumFractionDigits: 2 }),
                })}{" "}
            {p.by_notice_minor > 0 && t("taxes.byNotice", { amount: formatMinor(p.by_notice_minor, c), date: formatDate(year.notice_by) })}
          </p>
        )}
        {p.unvalued > 0 && <p className="text-xs text-amber-700 dark:text-amber-400">{t("taxes.unvalued", { count: p.unvalued })}</p>}
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("taxes.columns.account")}</TableHead>
                <TableHead className="text-right">{t("taxes.columns.sales")}</TableHead>
                <TableHead className="hidden text-right sm:table-cell">{t("taxes.columns.coupons")}</TableHead>
                <TableHead className="hidden text-right sm:table-cell">{t("taxes.columns.dividends")}</TableHead>
                <TableHead className="text-right">{t("taxes.columns.agent")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {p.accounts.filter((a) => !a.bank).map((a) => (
                <TableRow key={a.account_id} data-testid="tax-account">
                  <TableCell className="whitespace-normal">
                    {a.name}
                    {a.foreign_broker && <div className="text-xs text-muted-foreground">{t("taxes.foreignBroker")}</div>}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {formatMinor(a.sales_minor, c)}
                    {a.sales_long_minor > 0 && (
                      <div className="min-w-32 whitespace-normal text-xs text-muted-foreground">
                        {t("taxes.salesLong", { amount: formatMinor(a.sales_long_minor, c) })}
                      </div>
                    )}
                  </TableCell>
                  <TableCell className="hidden text-right tabular-nums sm:table-cell">{formatMinor(a.coupons_minor, c)}</TableCell>
                  <TableCell className="hidden text-right tabular-nums sm:table-cell">{formatMinor(a.dividends_minor, c)}</TableCell>
                  <TableCell className="text-right tabular-nums">
                    {a.foreign_broker ? (
                      <span className="text-xs text-muted-foreground">{t("taxes.selfPaid")}</span>
                    ) : (
                      formatMinor(a.agent_tax_minor, c)
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      </CardContent>
    </Card>
  );
}

// WagePayments lists a person's wage payments under a fold, each with its
// gross and withheld tax and where they come from; an editor writes them from
// the payslip, or fixes the program's reckoning as it is (decision Р-41).
function WagePayments({ payments, currency }: { payments: TaxYearWagePayment[]; currency: string }) {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  const entry = useWageEntry();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  if (payments.length === 0) return null;
  return (
    <div className="grid gap-2" data-testid="tax-wage-payments">
      <Button variant="link" className="h-auto justify-start p-0 text-xs" onClick={() => setOpen(!open)} aria-expanded={open}>
        {t("taxes.wagePayments", { count: payments.length })}
      </Button>
      {open && (
        <ul className="grid gap-2 text-xs">
          <li className="text-muted-foreground">{t("taxes.wageHint")}</li>
          {payments.map((w) => (
            <li key={w.operation_id} className="grid gap-1" data-testid="tax-wage-payment">
              <div className="flex flex-wrap items-center gap-x-2">
                <span>
                  {t("taxes.wagePayment", {
                    date: formatDate(w.on),
                    employer: w.employer || t("taxes.noEmployer"),
                    net: formatMinor(w.net_minor, currency),
                    gross: formatMinor(w.gross_minor, currency),
                    withheld: formatMinor(w.withheld_minor, currency),
                  })}
                </span>
                <span className="text-muted-foreground">· {w.entered ? t("taxes.wageEntered") : t("taxes.wageReckoned")}</span>
                {canEdit && editing !== w.operation_id && (
                  <>
                    <Button variant="link" className="h-auto p-0 text-xs" onClick={() => setEditing(w.operation_id)}>
                      {t("taxes.wageEnter")}
                    </Button>
                    {w.entered ? (
                      <Button variant="link" className="h-auto p-0 text-xs" disabled={entry.isPending}
                        onClick={() => entry.mutate({ operationId: w.operation_id, entry: null })}>
                        {t("taxes.wageUnenter")}
                      </Button>
                    ) : (
                      <Button variant="link" className="h-auto p-0 text-xs" disabled={entry.isPending}
                        onClick={() => entry.mutate({ operationId: w.operation_id, entry: { gross_minor: w.gross_minor, withheld_minor: w.withheld_minor } })}>
                        {t("taxes.wageAccept")}
                      </Button>
                    )}
                  </>
                )}
              </div>
              {editing === w.operation_id && <WageForm payment={w} onDone={() => setEditing(null)} />}
            </li>
          ))}
          {entry.isError && <li className="text-destructive">{t("taxes.wageFailed")}</li>}
        </ul>
      )}
    </div>
  );
}

function WageForm({ payment, onDone }: { payment: TaxYearWagePayment; onDone: () => void }) {
  const { t } = useTranslation();
  const [gross, setGross] = useState(minorToInput(payment.gross_minor));
  const [withheld, setWithheld] = useState(minorToInput(payment.withheld_minor));
  const entry = useWageEntry();
  const g = parseToMinor(gross);
  const w = parseToMinor(withheld);
  const ok = g !== null && w !== null && g > 0 && w >= 0 && w <= g;
  const id = payment.operation_id;
  return (
    <form
      className="flex flex-wrap items-end gap-2"
      onSubmit={(e) => {
        e.preventDefault();
        if (ok) entry.mutate({ operationId: id, entry: { gross_minor: g, withheld_minor: w } }, { onSuccess: onDone });
      }}
    >
      <div className="grid gap-1">
        <Label htmlFor={`wage-gross-${id}`}>{t("taxes.wageGross")}</Label>
        <Input id={`wage-gross-${id}`} inputMode="decimal" className="w-36" value={gross} onChange={(e) => setGross(e.target.value)} />
      </div>
      <div className="grid gap-1">
        <Label htmlFor={`wage-withheld-${id}`}>{t("taxes.wageWithheld")}</Label>
        <Input id={`wage-withheld-${id}`} inputMode="decimal" className="w-36" value={withheld} onChange={(e) => setWithheld(e.target.value)} />
      </div>
      <Button type="submit" size="sm" disabled={!ok || entry.isPending}>
        {t("common.save")}
      </Button>
      <Button type="button" size="sm" variant="outline" onClick={onDone}>
        {t("common.cancel")}
      </Button>
      {!ok && <p className="basis-full text-destructive">{t("taxes.wageBad")}</p>}
      {entry.isError && <p className="basis-full text-destructive">{t("taxes.wageFailed")}</p>}
    </form>
  );
}

// SharedAccounts asks, once, whose a shared broker's account is for tax:
// tax is a person's, the account's money the family's.
function SharedAccounts() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const accounts = useAccounts();
  const members = useMembers();
  const update = useUpdateAccount();
  const queryClient = useQueryClient();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const shared = (accounts.data ?? []).filter((a) => a.type === "brokerage" && a.status === "active" && a.owner_user_id == null);
  if (shared.length === 0 || !members.data) return null;
  const owner = members.data.find((m) => m.role === "owner");
  return (
    <Card data-testid="tax-shared">
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{t("taxes.sharedTitle")}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-2">
        <p className="text-sm text-muted-foreground">{t("taxes.sharedHint", { owner: owner?.display_name ?? "" })}</p>
        {shared.map((a) => (
          <div key={a.id} className="flex flex-wrap items-center justify-between gap-2">
            <span className="text-sm">{a.name}</span>
            <Select
              value={a.tax_owner_user_id ?? owner?.id ?? ""}
              disabled={!canEdit}
              onValueChange={(v) =>
                update.mutate(
                  { id: a.id, body: { tax_owner_user_id: v === owner?.id ? null : v } },
                  { onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["tax-year"] }) },
                )
              }
            >
              <SelectTrigger className="w-56" aria-label={t("taxes.sharedWhose", { account: a.name })}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {members.data.map((m) => (
                  <SelectItem key={m.id} value={m.id}>
                    {m.display_name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        ))}
      </CardContent>
    </Card>
  );
}
