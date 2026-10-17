import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { Link } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import type { AccountWithBalance } from "@/api/accounts";
import { useAccountIncome, type AccountIncome } from "@/api/account-income";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";

// IncomePanel is what a deposit or savings account brings by its rate and its
// share of its person's tax on interest (decision Р-56 Б): the lines on the
// money today, this year's income as «Налоги» count it, and the person's tax
// split between the accounts by their interest.
export function IncomePanel({ account }: { account: AccountWithBalance }) {
  const { t } = useTranslation();
  const q = useAccountIncome(account.id);
  if (q.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{t("app.error")}</AlertDescription>
      </Alert>
    );
  }
  const d = q.data;
  if (!d) return null;
  const lines = rows(t, d);
  if (lines.length === 0 && d.year_minor === null && d.estimate === null) {
    return <p className="text-sm text-muted-foreground" data-testid="account-income">{t("depositIncome.none")}</p>;
  }
  return (
    <div className="grid gap-3" data-testid="account-income">
      {lines.length > 0 && (
        <dl className="grid gap-1 text-sm">
          {lines.map(([label, value]) => (
            <div key={label} className="flex justify-between gap-3">
              <dt className="min-w-0">{label}</dt>
              <dd className="shrink-0 tabular-nums">{value}</dd>
            </div>
          ))}
        </dl>
      )}
      <Notes income={d} />
      <TaxShare income={d} />
    </div>
  );
}

// rows are the income's lines, label and amount, those known.
function rows(t: TFunction, d: AccountIncome): [string, string][] {
  const c = d.currency;
  const out: [string, string][] = [];
  if (d.term_minor !== null && d.ends_on !== null) out.push([t("depositIncome.term", { date: formatDate(d.ends_on) }), formatMinor(d.term_minor, c)]);
  if (d.accrued_minor !== null) out.push([t("depositIncome.accrued"), formatMinor(d.accrued_minor, c)]);
  if (d.at_end_minor !== null && d.ends_on !== null) out.push([t("depositIncome.atEnd", { date: formatDate(d.ends_on) }), formatMinor(d.at_end_minor, c)]);
  if (d.month_minor !== null && d.month_on !== null) out.push([t("depositIncome.month", { date: formatDate(d.month_on) }), formatMinor(d.month_minor, c)]);
  if (d.year_end_minor !== null) out.push([t("depositIncome.yearEnd"), formatMinor(d.year_end_minor, c)]);
  if (d.twelve_minor !== null) out.push([t("depositIncome.twelve"), formatMinor(d.twelve_minor, c)]);
  if (d.year_minor !== null && d.tax_currency !== "") out.push([t("depositIncome.year", { year: d.year }), formatMinor(d.year_minor, d.tax_currency)]);
  return out;
}

// Notes say what the lines stand on, and why something is not known.
function Notes({ income: d }: { income: AccountIncome }) {
  const { t } = useTranslation();
  const notes: string[] = [];
  if (d.rated) {
    notes.push(t("depositIncome.byRate"));
    if (d.payout === "end") notes.push(t("depositIncome.paysEnd"));
    if (d.payout === "monthly") notes.push(t("depositIncome.paysMonthly"));
    if (d.payout === "capitalize") notes.push(t("depositIncome.paysCapitalize"));
    if (d.ends_on !== null) notes.push(t("depositIncome.afterTerm"));
  } else {
    notes.push(t("depositIncome.notRated"));
  }
  const e = d.estimate;
  if (e !== null) {
    if (e.from !== null) notes.push(t("taxes.deposit.from", { date: formatDate(e.from) }));
    if (e.missing === "rate" && e.missing_on !== null) notes.push(t("taxes.deposit.missingRate", { date: formatDate(e.missing_on) }));
    if (e.missing === "how_paid") notes.push(t("taxes.deposit.missingHowPaid"));
    if (e.missing === "currency") notes.push(t("taxes.deposit.missingCurrency"));
    if (e.pays_on !== null) notes.push(t("taxes.deposit.paysOn", { date: formatDate(e.pays_on) }));
  }
  return <p className="text-xs text-muted-foreground" data-testid="account-income-notes">{notes.map((n) => `${n.charAt(0).toUpperCase()}${n.slice(1)}.`).join(" ")}</p>;
}

// TaxShare is the person's tax on interest at the year's end and this
// account's part of it, said to be a split the program makes.
function TaxShare({ income: d }: { income: AccountIncome }) {
  const { t } = useTranslation();
  const x = d.tax;
  if (x === null || d.year_minor === null) return null;
  const c = d.tax_currency;
  return (
    <div className="grid gap-1 text-sm" data-testid="account-income-tax">
      <p>
        {x.free_minor === null
          ? t("depositIncome.taxNoRate", { person: x.person, year: d.year, interest: formatMinor(x.interest_minor, c) })
          : x.tax_minor > 0
            ? t("depositIncome.tax", { person: x.person, year: d.year, tax: formatMinor(x.tax_minor, c), interest: formatMinor(x.interest_minor, c), free: formatMinor(x.free_minor, c) })
            : t("depositIncome.taxFree", { person: x.person, year: d.year, interest: formatMinor(x.interest_minor, c), free: formatMinor(x.free_minor, c) })}
      </p>
      {x.free_minor !== null && x.tax_minor > 0 && <p>{t("depositIncome.share", { share: formatMinor(x.share_minor, c) })}</p>}
      {x.unestimated.length > 0 && <p className="text-xs text-amber-700 dark:text-amber-400">{t("depositIncome.unestimated", { names: x.unestimated.join(", ") })}</p>}
      <p className="text-xs">
        <Link to="/taxes" className="underline">
          {t("depositIncome.toTaxes")}
        </Link>
      </p>
    </div>
  );
}
