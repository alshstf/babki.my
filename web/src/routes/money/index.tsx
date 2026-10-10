import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { useCashflow, type CashflowFlow, type CashflowLine, type CashflowSection } from "@/api/cashflow";
import { useCategories, type Category } from "@/api/categories";
import { useMembers } from "@/api/members";
import { useFileByRules } from "@/api/operations";
import { useSession } from "@/api/session";
import { FileByRulesBar } from "@/routes/accounts/operations-table";
import { QueryGate, RefreshFailedNotice } from "@/components/query-notice";
import { queryState, refreshFailed } from "@/lib/query-state";
import { formatMinorCompact } from "@/lib/money";
import { cn } from "@/lib/utils";
import { PERIODS, periodDays, type Period } from "@/lib/periods";

const EVERYONE = "all";
// Past this many months a table shows the totals only: the columns would not
// fit a screen. A phone shows the totals only whatever the period.
const MAX_MONTH_COLUMNS = 6;

// MoneyPage is the family's money over a period: what came in and went out by
// category and month, what is still unfiled, and what moved between the family
// and its brokers.
export function MoneyPage() {
  const { t } = useTranslation();
  const [period, setPeriod] = useState<Period>("last3");
  const [member, setMember] = useState(EVERYONE);
  const days = periodDays(period);
  const report = useCashflow({ ...days, member: member === EVERYONE ? undefined : member });
  const categories = useCategories();
  const members = useMembers();
  const { data: session } = useSession();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const fileByRules = useFileByRules();
  const state = queryState(report);
  const data = report.data;

  return (
    <div className="grid gap-6">
      <div className="grid gap-1">
        <h1 className="text-2xl font-bold">{t("money.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("money.hint")}</p>
      </div>
      <div className="flex flex-wrap gap-2">
        <Select value={period} onValueChange={(v) => setPeriod(v as Period)}>
          <SelectTrigger className="w-48" aria-label={t("money.period")}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PERIODS.map((p) => (
              <SelectItem key={p} value={p}>
                {t(`money.periods.${p}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={member} onValueChange={setMember}>
          <SelectTrigger className="w-48" aria-label={t("money.whose")}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={EVERYONE}>{t("money.everyone")}</SelectItem>
            <SelectItem value="shared">{t("money.shared")}</SelectItem>
            {(members.data ?? []).map((m) => (
              <SelectItem key={m.id} value={m.id}>
                {t("money.personal", { name: m.display_name })}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <RefreshFailedNotice show={refreshFailed(report)} />
      <QueryGate state={state} />
      {state === "ready" && data && (
        <>
          <Totals income={data.income.total_minor} expense={data.expense.total_minor} currency={data.base_currency} />
          {data.missing_rates.length > 0 && (
            <Alert data-testid="money-missing-rates">
              <AlertDescription>
                {t("money.missingRates", { rows: data.left_out, currencies: data.missing_rates.join(", ") })}
              </AlertDescription>
            </Alert>
          )}
          {(data.unfiled_in.total_minor !== 0 || data.unfiled_out.total_minor !== 0) && (
            <Alert data-testid="money-unfiled">
              <AlertDescription>
                {t("money.unfiled", {
                  in: formatMinorCompact(data.unfiled_in.total_minor, data.base_currency),
                  out: formatMinorCompact(data.unfiled_out.total_minor, data.base_currency),
                })}{" "}
                <Link to="/accounts" className="underline">
                  {t("money.unfiledLink")}
                </Link>
                {canEdit && (
                  <div className="mt-2">
                    <FileByRulesBar
                      pending={fileByRules.isPending}
                      filed={fileByRules.isSuccess ? fileByRules.data : undefined}
                      failed={fileByRules.isError}
                      onFile={() => fileByRules.mutate(undefined)}
                    />
                  </div>
                )}
              </AlertDescription>
            </Alert>
          )}
          <SectionTable
            title={t("money.expense")}
            section={data.expense}
            months={data.months}
            currency={data.base_currency}
            categories={categories.data ?? []}
            testId="money-expense"
          />
          <SectionTable
            title={t("money.income")}
            section={data.income}
            months={data.months}
            currency={data.base_currency}
            categories={categories.data ?? []}
            testId="money-income"
          />
          <Investments report={data.investments} currency={data.base_currency} />
        </>
      )}
    </div>
  );
}

function Totals({ income, expense, currency }: { income: number; expense: number; currency: string }) {
  const { t } = useTranslation();
  const saved = income - expense;
  // The share of what came in that stayed; meaningless without income.
  const rate = income > 0 ? Math.round((saved / income) * 100) : null;
  const card = (label: string, value: string, testId: string, tone?: string) => (
    <Card data-testid={testId}>
      <CardHeader className="pb-1">
        <CardTitle className="text-sm font-normal text-muted-foreground">{label}</CardTitle>
      </CardHeader>
      <CardContent className={cn("text-xl font-semibold tabular-nums sm:text-2xl", tone)}>{value}</CardContent>
    </Card>
  );
  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
      {card(t("money.income"), formatMinorCompact(income, currency), "money-total-income")}
      {card(t("money.expense"), formatMinorCompact(expense, currency), "money-total-expense")}
      {card(
        t("money.saved"),
        formatMinorCompact(saved, currency),
        "money-total-saved",
        saved < 0 ? "text-red-600 dark:text-red-400" : undefined,
      )}
      {card(t("money.savingsRate"), rate === null ? "—" : `${rate} %`, "money-savings-rate")}
    </div>
  );
}

// The name a line goes by: its category's, a group's, or a placeholder for a
// category the list does not hold (removed while the report was open).
function lineName(t: (key: string) => string, line: CashflowLine, categories: Category[]): string {
  if (line.group) return t(`money.groups.${line.group}`);
  return categories.find((c) => c.id === line.category_id)?.name ?? t("money.unknownCategory");
}

function SectionTable({
  title,
  section,
  months,
  currency,
  categories,
  testId,
}: {
  title: string;
  section: CashflowSection;
  months: string[];
  currency: string;
  categories: Category[];
  testId: string;
}) {
  const { t } = useTranslation();
  const byMonth = months.length > 1 && months.length <= MAX_MONTH_COLUMNS;
  // A share that rounds to nothing is still something: «<1 %», not «0 %».
  const share = (amount: number) => {
    if (section.total_minor <= 0) return "—";
    const percent = Math.round((amount / section.total_minor) * 100);
    return amount > 0 && percent === 0 ? "<1 %" : `${percent} %`;
  };
  // «Авг», «Сент»; with the year only when the period crosses one.
  const multiYear = months.length > 0 && months[0].slice(0, 4) !== months[months.length - 1].slice(0, 4);
  const monthLabel = (m: string) => {
    const name = new Date(`${m}-01T00:00:00`).toLocaleDateString("ru-RU", { month: "short" }).replace(".", "");
    const label = name.charAt(0).toUpperCase() + name.slice(1);
    return multiYear ? `${label} ${m.slice(0, 4)}` : label;
  };
  const row = (name: string, flow: CashflowFlow, key: string, level: 0 | 1, strong = false) => (
    <TableRow key={key} data-testid="money-line">
      <TableCell className={cn("whitespace-normal", level === 1 && "pl-8 text-muted-foreground", strong && "font-semibold")}>
        {name}
      </TableCell>
      {byMonth &&
        flow.by_month.map((v, i) => (
          <TableCell key={months[i]} className="hidden text-right tabular-nums sm:table-cell">
            {v === 0 ? "—" : formatMinorCompact(v, currency)}
          </TableCell>
        ))}
      <TableCell className={cn("text-right tabular-nums", strong && "font-semibold")}>
        {formatMinorCompact(flow.total_minor, currency)}
      </TableCell>
      <TableCell className="text-right tabular-nums text-muted-foreground">{share(flow.total_minor)}</TableCell>
    </TableRow>
  );

  return (
    <Card data-testid={testId}>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
      </CardHeader>
      <CardContent className="overflow-x-auto">
        {section.lines.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("money.nothing")}</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("money.category")}</TableHead>
                {byMonth &&
                  months.map((m) => (
                    <TableHead key={m} className="hidden text-right sm:table-cell">
                      {monthLabel(m)}
                    </TableHead>
                  ))}
                <TableHead className="text-right">{t("money.total")}</TableHead>
                <TableHead className="text-right">{t("money.share")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {section.lines.flatMap((line) => {
                const name = lineName(t, line, categories);
                const key = line.category_id ?? line.group ?? name;
                const rows = [row(name, line, key, 0)];
                // What was filed under the parent itself, beside its children.
                if (line.children.length > 0 && line.direct.total_minor !== 0) {
                  rows.push(row(t("money.direct"), line.direct, `${key}-direct`, 1));
                }
                for (const child of line.children) {
                  rows.push(row(lineName(t, child, categories), child, `${key}-${child.category_id}`, 1));
                }
                return rows;
              })}
              {row(t("money.total"), section, "total", 0, true)}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

function Investments({
  report,
  currency,
}: {
  report: { deposited: CashflowFlow; withdrawn: CashflowFlow; payouts: CashflowFlow; interest: CashflowFlow; costs: CashflowFlow };
  currency: string;
}) {
  const { t } = useTranslation();
  const items = [
    ["deposited", report.deposited],
    ["withdrawn", report.withdrawn],
    ["payouts", report.payouts],
    ["interest", report.interest],
    ["costs", report.costs],
  ] as const;
  if (items.every(([, flow]) => flow.total_minor === 0)) return null;
  return (
    <Card data-testid="money-investments">
      <CardHeader>
        <CardTitle>{t("money.investments.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("money.investments.hint")}</p>
      </CardHeader>
      <CardContent>
        <dl className="grid gap-x-6 gap-y-2 sm:grid-cols-2 lg:grid-cols-5">
          {items.map(([key, flow]) => (
            <div key={key}>
              <dt className="text-sm text-muted-foreground">{t(`money.investments.${key}`)}</dt>
              <dd className="text-lg font-semibold tabular-nums">{formatMinorCompact(flow.total_minor, currency)}</dd>
            </div>
          ))}
        </dl>
      </CardContent>
    </Card>
  );
}
