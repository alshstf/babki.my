import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useMembers } from "@/api/members";
import { useTaxYearForeign, type TaxYearForeignAccount } from "@/api/tax-year";
import { countryName } from "@/lib/country";
import { formatDate } from "@/lib/dates";
import { formatMinor } from "@/lib/money";
import { cn } from "@/lib/utils";

// ForeignReturn lays out each account at a broker abroad as the tax return
// lists it (decision Р-45): Приложение 2's lines for every payment, the sales
// with their lots for Приложение 8, and a table to save beside the return.
export function ForeignReturn({ year }: { year: number }) {
  const { t } = useTranslation();
  const q = useTaxYearForeign(year);
  const members = useMembers();
  const data = q.data;
  if (!data || !data.supported || data.accounts.length === 0) return null;
  const c = data.currency;
  const person = (id: string) => members.data?.find((m) => m.id === id)?.display_name ?? "";
  return (
    <Card data-testid="tax-foreign">
      <CardHeader className="grid gap-1">
        <CardTitle>{t("taxReturn.title", { year: data.year })}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("taxReturn.hint")}</p>
      </CardHeader>
      <CardContent className="grid gap-6">
        {data.accounts.map((a) => (
          <AccountLines key={a.account_id} account={a} currency={c} year={data.year} person={person(a.user_id)} />
        ))}
        <div className="grid gap-1 text-xs text-muted-foreground">
          <p>{data.treaties_as_of ? t("taxReturn.creditByTreaty", { date: formatDate(data.treaties_as_of) }) : t("taxReturn.credit")}</p>
          <p>{t("taxReturn.coupons")}</p>
          <p>{t("taxReturn.country")}</p>
        </div>
      </CardContent>
    </Card>
  );
}

function AccountLines({ account: a, currency: c, year, person }: { account: TaxYearForeignAccount; currency: string; year: number; person: string }) {
  const { t } = useTranslation();
  const broker = a.country ? `${countryName(a.country)}${a.country_code ? ` (${a.country_code})` : ""}` : t("taxReturn.brokerUnknown");
  return (
    <div className="grid gap-3" data-testid="tax-foreign-account">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div>
          <p className="font-medium">{a.name}</p>
          <p className="text-xs text-muted-foreground">{t("taxReturn.accountOf", { person, broker })}</p>
        </div>
        <Button type="button" size="sm" variant="outline" onClick={() => download(`${a.name}-${year}.csv`, toCSV(t, a))}>
          {t("taxReturn.download")}
        </Button>
      </div>
      {a.unvalued > 0 && <p className="text-xs text-amber-700 dark:text-amber-400">{t("taxReturn.unvalued", { count: a.unvalued })}</p>}
      {a.payouts.length > 0 && (
        <div className="grid gap-1">
          <p className="text-sm font-medium">{t("taxReturn.payouts")}</p>
          <div className="overflow-x-auto">
            <Table data-testid="tax-foreign-payouts">
              <TableHeader>
                <TableRow>
                  <TableHead>{t("taxReturn.col.on")}</TableHead>
                  <TableHead>{t("taxReturn.col.source")}</TableHead>
                  <TableHead>{t("taxReturn.col.kind")}</TableHead>
                  <TableHead>{t("taxReturn.col.country")}</TableHead>
                  <TableHead className="text-right">{t("taxReturn.col.amount")}</TableHead>
                  <TableHead className="text-right">{t("taxReturn.col.rate")}</TableHead>
                  <TableHead className="text-right">{t("taxReturn.col.inBase", { currency: c })}</TableHead>
                  <TableHead className="text-right">{t("taxReturn.col.withheld")}</TableHead>
                  <TableHead className="text-right">{t("taxReturn.col.credit")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {a.payouts.map((p, i) => (
                  <TableRow key={`${p.on}-${i}`} data-testid="tax-foreign-payout">
                    <TableCell className="tabular-nums">{formatDate(p.on)}</TableCell>
                    <TableCell className="whitespace-normal">{p.paper || payoutName(t, p.type)}</TableCell>
                    <TableCell className="tabular-nums">{p.kind}</TableCell>
                    <TableCell className="whitespace-normal">{p.country ? `${countryName(p.country)}${p.country_code ? ` (${p.country_code})` : ""}` : "—"}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatMinor(p.amount_minor, p.currency)}</TableCell>
                    <TableCell className="text-right tabular-nums">{p.rate ? p.rate.replace(".", ",") : "—"}</TableCell>
                    <TableCell className="text-right tabular-nums">{p.rate ? formatMinor(p.amount_base_minor, c) : "—"}</TableCell>
                    <TableCell className="text-right tabular-nums">{p.withheld_minor > 0 ? formatMinor(p.withheld_base_minor, c) : "—"}</TableCell>
                    <TableCell className="text-right tabular-nums">
                      {p.credit_minor > 0 ? formatMinor(p.credit_minor, c) : "—"}
                      {p.withheld_minor > 0 && treatyNote(t, p.treaty, p.treaty_from, p.country) && (
                        <div
                          className={cn("min-w-40 whitespace-normal text-xs", p.treaty === "partly" ? "text-muted-foreground" : "text-amber-700 dark:text-amber-400")}
                          data-testid="tax-foreign-treaty"
                        >
                          {treatyNote(t, p.treaty, p.treaty_from, p.country)}
                        </div>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </div>
      )}
      {a.sales.length > 0 && (
        <div className="grid gap-1">
          <p className="text-sm font-medium">{t("taxReturn.sales")}</p>
          <div className="overflow-x-auto">
            <Table data-testid="tax-foreign-sales">
              <TableHeader>
                <TableRow>
                  <TableHead>{t("taxReturn.col.on")}</TableHead>
                  <TableHead>{t("taxReturn.col.paper")}</TableHead>
                  <TableHead className="text-right">{t("taxReturn.col.proceeds")}</TableHead>
                  <TableHead>{t("taxReturn.col.lots")}</TableHead>
                  <TableHead className="text-right">{t("taxReturn.col.result", { currency: c })}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {a.sales.map((s, i) => (
                  <TableRow key={`${s.on}-${i}`} data-testid="tax-foreign-sale">
                    <TableCell className="tabular-nums">{formatDate(s.on)}</TableCell>
                    <TableCell className="whitespace-normal">
                      {s.paper}
                      {s.quantity !== "0" && <div className="text-xs text-muted-foreground">{t("taxReturn.quantity", { quantity: s.quantity })}</div>}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatMinor(s.proceeds_minor, s.currency)}
                      {s.rate && (
                        <div className="text-xs text-muted-foreground">
                          {t("taxReturn.atRate", { rate: s.rate.replace(".", ","), date: formatDate(s.settled_on), amount: formatMinor(s.proceeds_base_minor, c) })}
                        </div>
                      )}
                      {s.fee_minor > 0 && <div className="text-xs text-muted-foreground">{t("taxReturn.fee", { amount: formatMinor(s.fee_base_minor, c) })}</div>}
                    </TableCell>
                    <TableCell className="whitespace-normal text-xs">
                      {s.lots.map((l, k) => (
                        <div key={k} className="tabular-nums">
                          {l.acquired_on
                            ? t("taxReturn.lot", { date: formatDate(l.acquired_on), quantity: l.quantity, rate: l.rate ? l.rate.replace(".", ",") : "—", amount: formatMinor(l.cost_base_minor, c) })
                            : t("taxReturn.lotNoDay", { quantity: l.quantity })}
                        </div>
                      ))}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {s.result_base_minor !== null ? formatMinor(s.result_base_minor, c) : "—"}
                      {s.long_base_minor !== 0 && s.result_base_minor !== null && (
                        <div className="text-xs text-muted-foreground">{t("taxReturn.long", { amount: formatMinor(s.long_base_minor, c) })}</div>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          <p className="text-sm" data-testid="tax-foreign-app8">
            {t("taxReturn.app8", { income: formatMinor(a.income_base_minor, c), expenses: formatMinor(a.expenses_base_minor, c) })}
          </p>
        </div>
      )}
      {a.other_taxes.length > 0 && (
        <p className="text-xs text-muted-foreground" data-testid="tax-foreign-other">
          {t("taxReturn.otherTaxes", { count: a.other_taxes.length })}
        </p>
      )}
    </div>
  );
}

// treatyNote says why a payment's tax abroad is credited or not (decision
// Р-50): nothing where the treaty applies whole; literal keys for
// scripts/check-i18n.mjs.
function treatyNote(t: (key: string, v?: Record<string, unknown>) => string, state: string, from: string | null, country: string): string | null {
  const v = { country: country ? countryName(country) : "", date: from ? formatDate(from) : "" };
  switch (state) {
    case "partly":
      return t("taxReturn.treaty.partly", v);
    case "suspended":
      return t("taxReturn.treaty.suspended", v);
    case "ended":
      return t("taxReturn.treaty.ended", v);
    case "none":
      return t("taxReturn.treaty.none", v);
    case "no_country":
      return t("taxReturn.treaty.noCountry");
    default:
      return null;
  }
}

// payoutName names a payment without a paper: the broker's interest.
function payoutName(t: (key: string) => string, type: string): string {
  switch (type) {
    case "dividend":
      return t("taxReturn.types.dividend");
    case "coupon":
      return t("taxReturn.types.coupon");
    default:
      return t("taxReturn.types.interest");
  }
}

// csvMinor writes minor units as a table program reads them in Russian:
// «-1234,56». Digits moved, nothing counted.
function csvMinor(minor: number): string {
  const sign = minor < 0 ? "-" : "";
  const digits = String(Math.abs(minor)).padStart(3, "0");
  return `${sign}${digits.slice(0, -2)},${digits.slice(-2)}`;
}

function csvCell(v: string): string {
  return /[";\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v;
}

// toCSV is the account's lines as a table to save beside the return: the
// payments with Приложение 2's line numbers, then the sales lot by lot.
function toCSV(t: (key: string, v?: Record<string, unknown>) => string, a: TaxYearForeignAccount): string {
  const rows: string[][] = [
    [t("taxReturn.csv.payouts")],
    [
      t("taxReturn.csv.on"), t("taxReturn.csv.source"), t("taxReturn.csv.kind"), t("taxReturn.csv.country"), t("taxReturn.csv.creditedIn"),
      t("taxReturn.csv.currency"), t("taxReturn.csv.amount"), t("taxReturn.csv.rate"), t("taxReturn.csv.inBase"),
      t("taxReturn.csv.withheld"), t("taxReturn.csv.withheldInBase"), t("taxReturn.csv.taxRU"), t("taxReturn.csv.credit"),
    ],
    ...a.payouts.map((p) => [
      formatDate(p.on), p.paper || payoutName(t, p.type), p.kind, p.country_code, a.country_code, p.currency_code || p.currency,
      csvMinor(p.amount_minor), (p.rate ?? "").replace(".", ","), csvMinor(p.amount_base_minor), csvMinor(p.withheld_minor),
      csvMinor(p.withheld_base_minor), csvMinor(p.tax_ru_minor), csvMinor(p.credit_minor),
    ]),
    [],
    [t("taxReturn.csv.sales")],
    [
      t("taxReturn.csv.on"), t("taxReturn.csv.paper"), "ISIN", t("taxReturn.csv.quantity"), t("taxReturn.csv.currency"), t("taxReturn.csv.proceeds"),
      t("taxReturn.csv.fee"), t("taxReturn.csv.settledOn"), t("taxReturn.csv.rate"), t("taxReturn.csv.proceedsInBase"), t("taxReturn.csv.feeInBase"),
      t("taxReturn.csv.boughtOn"), t("taxReturn.csv.lotQuantity"), t("taxReturn.csv.cost"), t("taxReturn.csv.lotRate"), t("taxReturn.csv.costInBase"),
    ],
    ...a.sales.flatMap((s) =>
      (s.lots.length > 0 ? s.lots : [null]).map((l, k) => [
        formatDate(s.on), s.paper, s.isin, s.quantity.replace(".", ","), s.currency_code || s.currency,
        k === 0 ? csvMinor(s.proceeds_minor) : "", k === 0 ? csvMinor(s.fee_minor) : "", formatDate(s.settled_on), (s.rate ?? "").replace(".", ","),
        k === 0 ? csvMinor(s.proceeds_base_minor) : "", k === 0 ? csvMinor(s.fee_base_minor) : "",
        l?.acquired_on ? formatDate(l.acquired_on) : "", l ? l.quantity.replace(".", ",") : "", l ? csvMinor(l.cost_minor) : "",
        (l?.rate ?? "").replace(".", ","), l ? csvMinor(l.cost_base_minor) : "",
      ]),
    ),
    [],
    [t("taxReturn.csv.app8Income"), csvMinor(a.income_base_minor)],
    [t("taxReturn.csv.app8Expenses"), csvMinor(a.expenses_base_minor)],
  ];
  // The byte-order mark lets a table program read the Cyrillic as UTF-8.
  return "﻿" + rows.map((r) => r.map(csvCell).join(";")).join("\r\n");
}

function download(name: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: "text/csv;charset=utf-8" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  URL.revokeObjectURL(url);
}
