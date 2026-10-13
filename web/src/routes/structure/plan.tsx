import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { useAccounts } from "@/api/accounts";
import { useWhatIf, type AllocationPlanOperation, type AllocationWhatIf } from "@/api/allocation";
import type { Instrument } from "@/api/instruments";
import { currencyName, kindName } from "@/lib/allocation-names";
import { countryName } from "@/lib/country";
import { isCurrencyCode } from "@/lib/currencies";
import { formatMinor, formatMinorCompact, parseToMinor } from "@/lib/money";
import { percent } from "@/lib/percent";
import { sectorName } from "@/lib/sectors";
import { cn } from "@/lib/utils";
import { InstrumentPicker } from "@/routes/accounts/instrument-picker";

type Kind = AllocationPlanOperation["kind"];

// A planned operation as typed, before it is sent.
interface Planned {
  kind: Kind;
  accountId: string;
  toAccountId: string;
  instrument: Instrument | null;
  currency: string;
  amount: string;
}

const KINDS: Kind[] = ["buy", "sell", "deposit", "withdraw", "transfer"];

// PlanCard answers «what if» for a plan of operations (decision Р-48): each
// cut the plan moves — kinds, currencies, countries, sectors — before and
// after against the family's targets and ceilings, closer or further, and the
// tax on its sales. Nothing is written.
export function PlanCard({ valuation }: { valuation: "liquid" | "full" }) {
  const { t } = useTranslation();
  const accounts = useAccounts();
  const whatIf = useWhatIf(valuation);
  const [ops, setOps] = useState<Planned[]>([]);
  const [draft, setDraft] = useState<Planned | null>(null);
  const active = (accounts.data ?? []).filter((a) => a.status === "active");
  const nameOf = (id: string) => active.find((a) => a.id === id)?.name ?? "";
  const start = () => {
    const broker = active.find((a) => a.type === "brokerage") ?? active[0];
    setDraft({ kind: "buy", accountId: broker?.id ?? "", toAccountId: "", instrument: null, currency: broker?.currency ?? "RUB", amount: "" });
  };
  const evaluate = (list: Planned[]) =>
    whatIf.mutate(
      list.map((o) => ({
        kind: o.kind,
        account_id: o.accountId,
        to_account_id: o.kind === "transfer" ? o.toAccountId : null,
        instrument_id: o.kind === "buy" || o.kind === "sell" ? (o.instrument?.id ?? null) : null,
        currency: o.currency.toUpperCase(),
        amount_minor: parseToMinor(o.amount) ?? 0,
      })),
    );
  const add = (o: Planned) => {
    const list = [...ops, o];
    setOps(list);
    setDraft(null);
    evaluate(list);
  };
  const remove = (i: number) => {
    const list = ops.filter((_, k) => k !== i);
    setOps(list);
    if (list.length > 0) evaluate(list);
    else whatIf.reset();
  };
  if (active.length === 0) return null;
  return (
    <Card className="min-w-0" data-testid="plan">
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{t("plan.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("plan.hint")}</p>
      </CardHeader>
      <CardContent className="grid gap-4">
        {ops.length > 0 && (
          <ul className="grid gap-1 text-sm" data-testid="plan-ops">
            {ops.map((o, i) => (
              <li key={i} className="flex flex-wrap items-baseline justify-between gap-2">
                <span>{opLine(t, o, nameOf)}</span>
                <Button variant="link" className="h-auto p-0 text-xs" onClick={() => remove(i)}>
                  {t("plan.remove")}
                </Button>
              </li>
            ))}
          </ul>
        )}
        {draft ? (
          <OpForm draft={draft} accounts={active} onChange={setDraft} onAdd={add} onCancel={() => setDraft(null)} />
        ) : (
          <Button type="button" variant="outline" size="sm" className="justify-self-start" onClick={start}>
            {ops.length === 0 ? t("plan.start") : t("plan.add")}
          </Button>
        )}
        {whatIf.isError && (
          <Alert variant="destructive">
            <AlertDescription>{t("plan.failed")}</AlertDescription>
          </Alert>
        )}
        {whatIf.data && ops.length > 0 && <Result data={whatIf.data} />}
      </CardContent>
    </Card>
  );
}

function opLine(t: (key: string, v?: Record<string, unknown>) => string, o: Planned, nameOf: (id: string) => string): string {
  const amount = formatMinor(parseToMinor(o.amount) ?? 0, o.currency.toUpperCase());
  switch (o.kind) {
    case "buy":
      return t("plan.lineBuy", { account: nameOf(o.accountId), paper: o.instrument?.name ?? "", amount });
    case "sell":
      return t("plan.lineSell", { account: nameOf(o.accountId), paper: o.instrument?.name ?? "", amount });
    case "deposit":
      return t("plan.lineDeposit", { account: nameOf(o.accountId), amount });
    case "withdraw":
      return t("plan.lineWithdraw", { account: nameOf(o.accountId), amount });
    default:
      return t("plan.lineTransfer", { account: nameOf(o.accountId), to: nameOf(o.toAccountId), amount });
  }
}

function kindLabel(t: (key: string) => string, k: Kind): string {
  switch (k) {
    case "buy":
      return t("plan.kinds.buy");
    case "sell":
      return t("plan.kinds.sell");
    case "deposit":
      return t("plan.kinds.deposit");
    case "withdraw":
      return t("plan.kinds.withdraw");
    default:
      return t("plan.kinds.transfer");
  }
}

function OpForm({ draft, accounts, onChange, onAdd, onCancel }: {
  draft: Planned;
  accounts: { id: string; name: string; currency: string; type: string }[];
  onChange: (p: Planned) => void;
  onAdd: (p: Planned) => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  const paper = draft.kind === "buy" || draft.kind === "sell";
  const choices = paper ? accounts.filter((a) => a.type === "brokerage") : accounts;
  const minor = parseToMinor(draft.amount);
  const ok =
    draft.accountId !== "" &&
    minor !== null &&
    minor > 0 &&
    isCurrencyCode(draft.currency.toUpperCase()) &&
    (!paper || draft.instrument !== null) &&
    (draft.kind !== "transfer" || (draft.toAccountId !== "" && draft.toAccountId !== draft.accountId));
  return (
    <div className="grid gap-3 rounded-md border p-3" data-testid="plan-form">
      <div className="grid gap-1">
        <Label htmlFor="plan-kind">{t("plan.kind")}</Label>
        <Select value={draft.kind} onValueChange={(v) => onChange({ ...draft, kind: v as Kind })}>
          <SelectTrigger id="plan-kind" className="w-full sm:w-56">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {KINDS.map((k) => (
              <SelectItem key={k} value={k}>
                {kindLabel(t, k)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <div className="grid gap-1">
        <Label htmlFor="plan-account">{draft.kind === "transfer" ? t("plan.from") : t("plan.account")}</Label>
        <Select
          value={draft.accountId}
          onValueChange={(v) => onChange({ ...draft, accountId: v, currency: paper ? draft.currency : (accounts.find((a) => a.id === v)?.currency ?? draft.currency) })}
        >
          <SelectTrigger id="plan-account" className="w-full sm:w-72">
            <SelectValue placeholder={t("plan.pickAccount")} />
          </SelectTrigger>
          <SelectContent>
            {choices.map((a) => (
              <SelectItem key={a.id} value={a.id}>
                {a.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {draft.kind === "transfer" && (
        <div className="grid gap-1">
          <Label htmlFor="plan-to">{t("plan.to")}</Label>
          <Select value={draft.toAccountId} onValueChange={(v) => onChange({ ...draft, toAccountId: v })}>
            <SelectTrigger id="plan-to" className="w-full sm:w-72">
              <SelectValue placeholder={t("plan.pickAccount")} />
            </SelectTrigger>
            <SelectContent>
              {accounts.filter((a) => a.id !== draft.accountId).map((a) => (
                <SelectItem key={a.id} value={a.id}>
                  {a.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}
      {paper && (
        <div className="grid gap-1">
          <Label>{t("plan.paper")}</Label>
          <InstrumentPicker value={draft.instrument} onChange={(i) => onChange({ ...draft, instrument: i, currency: i.currency })} />
        </div>
      )}
      <div className="flex flex-wrap items-end gap-2">
        <div className="grid gap-1">
          <Label htmlFor="plan-amount">{t("plan.amount")}</Label>
          <Input id="plan-amount" inputMode="decimal" className="w-40" value={draft.amount} onChange={(e) => onChange({ ...draft, amount: e.target.value })} />
        </div>
        <div className="grid gap-1">
          <Label htmlFor="plan-currency">{t("plan.currency")}</Label>
          <Input id="plan-currency" className="w-20" maxLength={3} value={draft.currency} onChange={(e) => onChange({ ...draft, currency: e.target.value })} />
        </div>
      </div>
      <div className="flex gap-2">
        <Button size="sm" disabled={!ok} onClick={() => onAdd(draft)}>
          {t("plan.addToPlan")}
        </Button>
        <Button size="sm" variant="outline" onClick={onCancel}>
          {t("common.cancel")}
        </Button>
      </div>
    </div>
  );
}

const VERDICT_CLASS: Record<string, string> = {
  closer: "text-emerald-700 dark:text-emerald-400",
  further: "text-amber-700 dark:text-amber-400",
  same: "text-muted-foreground",
  none: "text-muted-foreground",
};

function Result({ data }: { data: AllocationWhatIf }) {
  const { t } = useTranslation();
  const [all, setAll] = useState(false);
  const c = data.base_currency;
  const title = (d: string) =>
    d === "kind" ? t("plan.cuts.kind") : d === "currency" ? t("plan.cuts.currency") : d === "country" ? t("plan.cuts.country") : t("plan.cuts.sector");
  const name = (d: string, k: string) =>
    d === "kind"
      ? kindName(t, k, c)
      : d === "currency"
        ? currencyName(t, k)
        : d === "country"
          ? k === "-" ? t("structure.noIsin") : countryName(k)
          : sectorName(t, k);
  const verdict = (v: string) =>
    v === "closer" ? t("plan.verdicts.closer") : v === "further" ? t("plan.verdicts.further") : v === "same" ? t("plan.verdicts.same") : t("plan.verdicts.none");
  const short = data.cuts.find((x) => x.dimension === "kind")?.rows.find((r) => r.key === "broker_cash" && r.after_minor < 0);
  return (
    <div className="grid gap-3" data-testid="plan-result">
      {short && <p className="text-sm text-amber-700 dark:text-amber-400">{t("plan.short", { amount: formatMinorCompact(-short.after_minor, c) })}</p>}
      {data.cuts.map((cut) => {
        const rows = cut.rows.filter((r) => all || r.before_minor !== r.after_minor);
        if (rows.length === 0) return null;
        return (
          <div key={cut.dimension} className="grid gap-1" data-testid={`plan-cut-${cut.dimension}`}>
            <p className="text-sm font-medium">{title(cut.dimension)}</p>
            {rows.map((r) => (
              <div key={r.key} className="grid grid-cols-[minmax(0,1fr)_auto] items-baseline gap-x-3 text-sm" data-testid="plan-row">
                <span className="min-w-0 break-words">{name(cut.dimension, r.key)}</span>
                <span className={cn("text-xs font-semibold", VERDICT_CLASS[r.verdict])}>{verdict(r.verdict)}</span>
                <span className="col-span-2 text-xs text-muted-foreground tabular-nums">
                  {percent(r.share_before)} → {percent(r.share_after)}
                  {r.target !== null &&
                    ` · ${cut.dimension === "country" || cut.dimension === "sector" ? t("plan.ceiling", { value: percent(r.target) }) : t("plan.target", { value: percent(r.target) })}`}
                </span>
              </div>
            ))}
          </div>
        );
      })}
      {data.sales.length > 0 && (
        <p className="text-sm" data-testid="plan-tax">
          {data.tax_minor !== null
            ? t("plan.tax", { tax: formatMinor(data.tax_minor, c), rate: data.tax_rate ?? "" })
            : t("plan.taxUnknown")}
        </p>
      )}
      <Button variant="link" className="h-auto justify-self-start p-0 text-xs" onClick={() => setAll(!all)}>
        {all ? t("plan.changedOnly") : t("plan.showAll")}
      </Button>
    </div>
  );
}
