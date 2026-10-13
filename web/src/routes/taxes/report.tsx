import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useMembers } from "@/api/members";
import { useTaxYearForeignReport, type TaxYearForeignReport } from "@/api/tax-year";
import { countryName } from "@/lib/country";
import { formatDate } from "@/lib/dates";
import { formatMinor } from "@/lib/money";
import { cn } from "@/lib/utils";

type Account = TaxYearForeignReport["accounts"][number];

// ForeignReport is the yearly report on the money's moves of each account at
// a broker abroad (decision Р-45, form КНД 1112520): the money by currency and
// the papers' worth as the form's sheets 3 and 4 ask them, in thousands, and
// whether the report is due at all.
export function ForeignReport({ year }: { year: number }) {
  const { t } = useTranslation();
  const q = useTaxYearForeignReport(year);
  const members = useMembers();
  const data = q.data;
  if (!data || !data.supported || data.accounts.length === 0) return null;
  const person = (id: string) => members.data?.find((m) => m.id === id)?.display_name ?? "";
  return (
    <Card data-testid="tax-report">
      <CardHeader className="grid gap-1">
        <CardTitle>{t("taxReport.title", { year: data.year, date: formatDate(data.due_by) })}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("taxReport.hint")}</p>
      </CardHeader>
      <CardContent className="grid gap-6">
        {data.accounts.map((a) => (
          <AccountReport key={a.account_id} account={a} report={data} person={person(a.user_id)} />
        ))}
        <p className="text-xs text-muted-foreground">{t("taxReport.abroad")}</p>
      </CardContent>
    </Card>
  );
}

function AccountReport({ account: a, report: r, person }: { account: Account; report: TaxYearForeignReport; person: string }) {
  const { t } = useTranslation();
  const broker = a.country ? `${countryName(a.country)}${a.country_code ? ` (${a.country_code})` : ""}` : t("taxReturn.brokerUnknown");
  const limit = formatMinor(r.free_up_to_minor, r.currency);
  const came = formatMinor(a.in_minor, r.currency);
  return (
    <div className="grid gap-3" data-testid="tax-report-account">
      <div>
        <p className="font-medium">{a.name}</p>
        <p className="text-xs text-muted-foreground">{t("taxReturn.accountOf", { person, broker })}</p>
      </div>
      <p
        className={cn("text-sm", a.verdict === "due" ? "font-medium" : a.verdict === "unknown" && "text-amber-700 dark:text-amber-400")}
        data-testid="tax-report-verdict"
      >
        {verdict(t, a.verdict, a.nothing_in ? t("taxReport.atEnd", { amount: came }) : t("taxReport.cameIn", { amount: came }), limit)}
      </p>
      <div className="overflow-x-auto">
        <Table data-testid="tax-report-cash">
          <TableHeader>
            <TableRow>
              <TableHead>{t("taxReport.col.currency")}</TableHead>
              <TableHead className="text-right">{t("taxReport.col.start")}</TableHead>
              <TableHead className="text-right">{t("taxReport.col.in")}</TableHead>
              <TableHead className="text-right">{t("taxReport.col.out")}</TableHead>
              <TableHead className="text-right">{t("taxReport.col.end")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {a.cash.map((c) => (
              <TableRow key={c.currency} data-testid="tax-report-currency">
                <TableCell>
                  {c.currency}
                  {c.currency_code && <span className="text-muted-foreground"> ({c.currency_code})</span>}
                </TableCell>
                <TableCell className="text-right tabular-nums">{c.start_thousands}</TableCell>
                <TableCell className="text-right tabular-nums">{c.in_thousands}</TableCell>
                <TableCell className="text-right tabular-nums">{c.out_thousands}</TableCell>
                <TableCell className="text-right tabular-nums">{c.end_thousands}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <p className="text-xs text-muted-foreground">{t("taxReport.cashNote")}</p>
      {(a.papers_start.length > 0 || a.papers_end.length > 0) && (
        <div className="grid gap-1 text-sm" data-testid="tax-report-papers">
          <p className="font-medium">{t("taxReport.papers")}</p>
          <p>
            {t("taxReport.papersStart", { value: a.papers_start.map((p) => `${p.thousands} ${p.currency}`).join(", ") || "0" })}
            {" · "}
            {t("taxReport.papersEnd", { value: a.papers_end.map((p) => `${p.thousands} ${p.currency}`).join(", ") || "0" })}
          </p>
          <p className="text-xs text-muted-foreground">{t("taxReport.papersNote")}</p>
          {a.unpriced > 0 && <p className="text-xs text-amber-700 dark:text-amber-400">{t("taxReport.unpriced", { count: a.unpriced })}</p>}
        </div>
      )}
    </div>
  );
}

// verdict is the words on whether the report is due; literal keys for
// scripts/check-i18n.mjs.
function verdict(t: (key: string, v?: Record<string, unknown>) => string, v: string, came: string, limit: string): string {
  switch (v) {
    case "free":
      return t("taxReport.verdict.free", { came, limit });
    case "free_if_listed":
      return t("taxReport.verdict.freeIfListed", { came, limit });
    case "due":
      return t("taxReport.verdict.due", { came, limit });
    default:
      return t("taxReport.verdict.unknown");
  }
}
