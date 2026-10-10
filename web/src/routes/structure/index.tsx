import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useStructure, type StructureSlice, type StructureValuation } from "@/api/structure";
import { useAccounts } from "@/api/accounts";
import { QueryGate, RefreshFailedNotice } from "@/components/query-notice";
import { queryState, refreshFailed } from "@/lib/query-state";
import { formatMinorCompact } from "@/lib/money";
import { countryName } from "@/lib/country";
import { cn } from "@/lib/utils";

// The words for each kind of asset; a switch with literal keys for
// scripts/check-i18n.mjs.
function className(t: (key: string) => string, key: string): string {
  switch (key) {
    case "shares":
      return t("structure.classes.shares");
    case "bonds":
      return t("structure.classes.bonds");
    case "funds":
      return t("structure.classes.funds");
    case "currency":
      return t("structure.classes.currency");
    case "metals":
      return t("structure.classes.metals");
    case "crypto":
      return t("structure.classes.crypto");
    case "broker_cash":
      return t("structure.classes.brokerCash");
    case "money":
      return t("structure.classes.money");
    case "deposits":
      return t("structure.classes.deposits");
    case "broker_balance":
      return t("structure.classes.brokerBalance");
    default:
      return t("structure.classes.other");
  }
}

// StructurePage is the family's worth taken apart: by kind of asset, currency,
// account and the country a paper was issued in; debts apart.
export function StructurePage() {
  const { t } = useTranslation();
  const [valuation, setValuation] = useState<StructureValuation>("liquid");
  const structure = useStructure(valuation);
  const accounts = useAccounts();
  const state = queryState(structure);
  const data = structure.data;
  const accountName = (id: string) => accounts.data?.find((a) => a.id === id)?.name ?? t("structure.unknownAccount");

  return (
    <div className="grid gap-6">
      <div className="grid gap-1">
        <h1 className="text-2xl font-bold">{t("structure.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("structure.hint")}</p>
      </div>
      <Select value={valuation} onValueChange={(v) => setValuation(v as StructureValuation)}>
        <SelectTrigger className="w-auto max-w-full" aria-label={t("structure.valuation")}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="liquid">{t("structure.liquid")}</SelectItem>
          <SelectItem value="full">{t("structure.full")}</SelectItem>
        </SelectContent>
      </Select>
      <RefreshFailedNotice show={refreshFailed(structure)} />
      <QueryGate state={state} />
      {state === "ready" && data && (
        <>
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-3">
            <Card data-testid="structure-assets">
              <CardHeader className="pb-1">
                <CardTitle className="text-sm font-normal text-muted-foreground">{t("structure.assets")}</CardTitle>
              </CardHeader>
              <CardContent className="text-xl font-semibold tabular-nums sm:text-2xl">
                {formatMinorCompact(data.assets_minor, data.base_currency)}
              </CardContent>
            </Card>
            <Card data-testid="structure-debts">
              <CardHeader className="pb-1">
                <CardTitle className="text-sm font-normal text-muted-foreground">{t("structure.debts")}</CardTitle>
              </CardHeader>
              <CardContent className={cn("text-xl font-semibold tabular-nums sm:text-2xl", data.debts_minor < 0 && "text-red-600 dark:text-red-400")}>
                {formatMinorCompact(data.debts_minor, data.base_currency)}
              </CardContent>
            </Card>
            <Card data-testid="structure-net">
              <CardHeader className="pb-1">
                <CardTitle className="text-sm font-normal text-muted-foreground">{t("structure.net")}</CardTitle>
              </CardHeader>
              <CardContent className="text-xl font-semibold tabular-nums sm:text-2xl">
                {formatMinorCompact(data.assets_minor + data.debts_minor, data.base_currency)}
              </CardContent>
            </Card>
          </div>
          {(data.unpriced > 0 || data.missing_rates.length > 0) && (
            <Alert>
              <AlertDescription>
                {data.unpriced > 0 && t("structure.unpriced", { count: data.unpriced })}{" "}
                {data.missing_rates.length > 0 && t("structure.missingRates", { currencies: data.missing_rates.join(", ") })}
              </AlertDescription>
            </Alert>
          )}
          <div className="grid gap-6 lg:grid-cols-2">
            <Breakdown title={t("structure.byClass")} slices={data.by_class} total={data.assets_minor} currency={data.base_currency} name={(k) => className(t, k)} testId="structure-class" />
            <Breakdown title={t("structure.byCurrency")} slices={data.by_currency} total={data.assets_minor} currency={data.base_currency} name={(k) => k} testId="structure-currency" />
            <Breakdown title={t("structure.byAccount")} slices={data.by_account} total={data.assets_minor} currency={data.base_currency} name={accountName} testId="structure-account" />
            <Breakdown
              title={t("structure.byCountry")}
              slices={data.by_country}
              total={data.assets_minor}
              currency={data.base_currency}
              name={(k) => (k === "" ? t("structure.notPapers") : k === "-" ? t("structure.noIsin") : countryName(k))}
              testId="structure-country"
              note={t("structure.countryNote")}
            />
          </div>
        </>
      )}
    </div>
  );
}

function Breakdown({
  title,
  slices,
  total,
  currency,
  name,
  testId,
  note,
}: {
  title: string;
  slices: StructureSlice[];
  total: number;
  currency: string;
  name: (key: string) => string;
  testId: string;
  note?: string;
}) {
  const share = (minor: number) => (total > 0 ? (minor / total) * 100 : 0);
  return (
    <Card data-testid={testId}>
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{title}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-2">
        {slices.map((s) => (
          <div key={s.key} className="grid gap-1" data-testid="structure-slice">
            <div className="flex items-baseline justify-between gap-3 text-sm">
              <span className="min-w-0 truncate">{name(s.key)}</span>
              <span className="shrink-0 tabular-nums">
                {formatMinorCompact(s.minor, currency)}{" "}
                <span className="text-muted-foreground">
                  {share(s.minor) < 1 && s.minor > 0 ? "<1" : Math.round(share(s.minor))} %
                </span>
              </span>
            </div>
            <div className="h-2 overflow-hidden rounded bg-muted">
              <div className="h-full rounded bg-emerald-500" style={{ width: `${share(s.minor)}%` }} />
            </div>
          </div>
        ))}
        {note && <p className="text-xs text-muted-foreground">{note}</p>}
      </CardContent>
    </Card>
  );
}
