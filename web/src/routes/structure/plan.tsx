import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { useAccounts } from "@/api/accounts";
import { useIntermediaries } from "@/api/intermediaries";
import { useMembers } from "@/api/members";
import { useSession } from "@/api/session";
import {
  rebalanceQuery,
  useDeletePlan,
  useSavePlan,
  useSavedPlans,
  useWhatIf,
  type AllocationPlanOperation,
  type AllocationSavedPlan,
  type AllocationWhatIf,
} from "@/api/allocation";
import { useInstrumentIndex, type Instrument } from "@/api/instruments";
import { currencyName, kindName } from "@/lib/allocation-names";
import { countryName } from "@/lib/country";
import { isCurrencyCode } from "@/lib/currencies";
import { formatDate } from "@/lib/dates";
import { formatMinor, formatMinorCompact, minorToInput, parseToMinor } from "@/lib/money";
import { percent } from "@/lib/percent";
import { sectorName } from "@/lib/sectors";
import { cn } from "@/lib/utils";
import { InstrumentPicker } from "@/routes/accounts/instrument-picker";

type Kind = AllocationPlanOperation["kind"];

// The paper of a purchase or sale: what the line needs to name it.
type Paper = Pick<Instrument, "id" | "name">;

// A planned operation as typed, before it is sent. suggested: filled from the
// rebalance's suggestion, its sum in the base currency whatever the paper's.
interface Planned {
  kind: Kind;
  accountId: string;
  toAccountId: string;
  instrument: Paper | null;
  currency: string;
  amount: string;
  suggested?: boolean;
}

// A kind's purchase or sale the rebalance suggests, waiting for its account
// and paper.
interface Suggested {
  kind: "buy" | "sell";
  key: string;
  minor: number;
}

const KINDS: Kind[] = ["buy", "sell", "deposit", "withdraw", "transfer"];

// The kinds a plan buys and sells papers of; the suggestion's other moves —
// currency, deposits as they end — are the family's to add.
const PAPER_KINDS = ["shares", "bonds", "funds", "metals"];

// PlanCard answers «what if» for a plan of operations (decision Р-48): each
// cut the plan moves — kinds, currencies, countries, sectors — before and
// after against the family's targets and ceilings, closer or further, and the
// tax on its sales. Nothing is written to the journal; a plan can be kept
// under a name and valued again at today's prices.
export function PlanCard({ valuation }: { valuation: "liquid" | "full" }) {
  const { t } = useTranslation();
  const accounts = useAccounts();
  const whatIf = useWhatIf(valuation);
  const { data: session } = useSession();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const [ops, setOps] = useState<Planned[]>([]);
  const [draft, setDraft] = useState<{ planned: Planned; pending?: number } | null>(null);
  const [pending, setPending] = useState<Suggested[]>([]);
  const [opened, setOpened] = useState<{ name: string; savedAt: string } | null>(null);
  const active = (accounts.data ?? []).filter((a) => a.status === "active");
  const nameOf = (id: string) => active.find((a) => a.id === id)?.name ?? "";
  const broker = active.find((a) => a.type === "brokerage") ?? active[0];
  const start = () =>
    setDraft({ planned: { kind: "buy", accountId: broker?.id ?? "", toAccountId: "", instrument: null, currency: broker?.currency ?? "RUB", amount: "" } });
  const evaluate = (list: Planned[]) => {
    if (list.length === 0) {
      whatIf.reset();
      return;
    }
    whatIf.mutate(list.map(toOperation));
  };
  const add = (o: Planned) => {
    const list = [...ops, o];
    setOps(list);
    if (draft?.pending !== undefined) setPending(pending.filter((_, k) => k !== draft.pending));
    setDraft(null);
    evaluate(list);
  };
  const remove = (i: number) => {
    const list = ops.filter((_, k) => k !== i);
    setOps(list);
    evaluate(list);
  };
  const open = (plan: AllocationSavedPlan, papers: Map<string, Instrument>) => {
    const list = plan.operations.map((o) => fromOperation(o, papers));
    setOps(list);
    setPending([]);
    setDraft(null);
    setOpened({ name: plan.name, savedAt: plan.saved_at });
    evaluate(list);
  };
  if (active.length === 0) return null;
  return (
    <Card className="min-w-0" data-testid="plan">
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{t("plan.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("plan.hint")}</p>
      </CardHeader>
      <CardContent className="grid gap-4">
        {opened && (
          <p className="text-xs text-muted-foreground" data-testid="plan-opened">
            {t("plan.opened", { name: opened.name, date: formatDate(opened.savedAt.slice(0, 10)) })}
          </p>
        )}
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
        {pending.length > 0 && (
          <ul className="grid gap-1 text-sm" data-testid="plan-pending">
            {pending.map((p, i) => (
              <li key={`${p.kind}-${p.key}`} className="flex flex-wrap items-baseline justify-between gap-2 text-muted-foreground">
                <span>{pendingLine(t, p, base(whatIf.data, broker?.currency))}</span>
                <span className="flex gap-3">
                  <Button
                    variant="link"
                    className="h-auto p-0 text-xs"
                    onClick={() =>
                      setDraft({
                        pending: i,
                        planned: { kind: p.kind, accountId: broker?.id ?? "", toAccountId: "", instrument: null, currency: base(whatIf.data, broker?.currency), amount: minorToInput(p.minor), suggested: true },
                      })
                    }
                  >
                    {t("plan.pickPaper")}
                  </Button>
                  <Button variant="link" className="h-auto p-0 text-xs" onClick={() => setPending(pending.filter((_, k) => k !== i))}>
                    {t("plan.remove")}
                  </Button>
                </span>
              </li>
            ))}
          </ul>
        )}
        {draft ? (
          <OpForm
            draft={draft.planned}
            accounts={active}
            onChange={(planned) => setDraft({ ...draft, planned })}
            onAdd={add}
            onCancel={() => setDraft(null)}
          />
        ) : (
          <div className="flex flex-wrap gap-2">
            <Button type="button" variant="outline" size="sm" onClick={start}>
              {ops.length === 0 ? t("plan.start") : t("plan.add")}
            </Button>
            <FromSuggestion valuation={valuation} onFill={setPending} />
          </div>
        )}
        {whatIf.isError && (
          <Alert variant="destructive">
            <AlertDescription>{t("plan.failed")}</AlertDescription>
          </Alert>
        )}
        {whatIf.data && ops.length > 0 && <Result data={whatIf.data} />}
        {canEdit && ops.length > 0 && <SavePlan key={opened?.name ?? ""} ops={ops} name={opened?.name ?? ""} onSaved={(p) => setOpened({ name: p.name, savedAt: p.saved_at })} />}
        <SavedPlans canEdit={canEdit} onOpen={open} />
      </CardContent>
    </Card>
  );
}

function pendingLine(t: (key: string, v?: Record<string, unknown>) => string, p: Suggested, currency: string): string {
  const v = { kind: kindName(t, p.key, currency), amount: formatMinorCompact(p.minor, currency) };
  return p.kind === "buy" ? t("plan.pendingBuy", v) : t("plan.pendingSell", v);
}

// The base currency the suggestion's sums are in: the plan's, once valued,
// else the broker's account's.
function base(data: AllocationWhatIf | undefined, fallback: string | undefined): string {
  return data?.base_currency ?? fallback ?? "RUB";
}

function toOperation(o: Planned): AllocationPlanOperation {
  return {
    kind: o.kind,
    account_id: o.accountId,
    to_account_id: o.kind === "transfer" ? o.toAccountId : null,
    instrument_id: o.kind === "buy" || o.kind === "sell" ? (o.instrument?.id ?? null) : null,
    currency: o.currency.toUpperCase(),
    amount_minor: parseToMinor(o.amount) ?? 0,
  };
}

function fromOperation(o: AllocationPlanOperation, papers: Map<string, Instrument>): Planned {
  const id = o.instrument_id ?? null;
  return {
    kind: o.kind,
    accountId: o.account_id,
    toAccountId: o.to_account_id ?? "",
    instrument: id ? { id, name: papers.get(id)?.name ?? "?" } : null,
    currency: o.currency,
    amount: minorToInput(o.amount_minor),
  };
}

// FromSuggestion fills the plan from the rebalance's suggestion (Р-37 В):
// the sums each kind of papers is to be bought or sold for, the account and
// the paper left to the family.
function FromSuggestion({ valuation, onFill }: { valuation: "liquid" | "full"; onFill: (list: Suggested[]) => void }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [state, setState] = useState<"idle" | "asking" | "empty" | "failed">("idle");
  const fill = async () => {
    setState("asking");
    try {
      const rebalance = await queryClient.fetchQuery(rebalanceQuery(0, valuation));
      const list: Suggested[] = rebalance.kinds
        .filter((k) => PAPER_KINDS.includes(k.key) && k.change_minor !== 0)
        .map((k) => ({ kind: k.change_minor > 0 ? "buy" : "sell", key: k.key, minor: Math.abs(k.change_minor) }));
      // Sales first: their money pays for the purchases.
      list.sort((a, b) => (a.kind === b.kind ? 0 : a.kind === "sell" ? -1 : 1));
      onFill(list);
      setState(list.length === 0 ? "empty" : "idle");
    } catch {
      setState("failed");
    }
  };
  return (
    <>
      <Button type="button" variant="ghost" size="sm" disabled={state === "asking"} onClick={() => void fill()}>
        {t("plan.fromSuggestion")}
      </Button>
      {state === "empty" && <p className="basis-full text-xs text-muted-foreground">{t("plan.suggestionEmpty")}</p>}
      {state === "failed" && <p className="basis-full text-xs text-red-700 dark:text-red-400">{t("plan.failed")}</p>}
    </>
  );
}

// SavePlan keeps the plan under a name; the same name replaces the one kept.
function SavePlan({ ops, name, onSaved }: { ops: Planned[]; name: string; onSaved: (plan: AllocationSavedPlan) => void }) {
  const { t } = useTranslation();
  const save = useSavePlan();
  // Keyed by the plan opened: its name is where typing starts.
  const [typed, setTyped] = useState(name);
  const trimmed = typed.trim();
  return (
    <div className="grid gap-1" data-testid="plan-save">
      <div className="flex flex-wrap items-end gap-2">
        <div className="grid gap-1">
          <Label htmlFor="plan-name">{t("plan.saveName")}</Label>
          <Input id="plan-name" className="w-56" maxLength={100} value={typed} onChange={(e) => setTyped(e.target.value)} />
        </div>
        <Button
          size="sm"
          variant="outline"
          disabled={trimmed === "" || save.isPending}
          onClick={() =>
            save.mutate(
              { name: trimmed, operations: ops.map(toOperation) },
              {
                onSuccess: (plans) => {
                  const kept = plans.find((x) => x.name === trimmed);
                  if (kept) onSaved(kept);
                },
              },
            )
          }
        >
          {t("plan.save")}
        </Button>
      </div>
      {save.isError && <p className="text-xs text-red-700 dark:text-red-400">{t("plan.saveFailed")}</p>}
    </div>
  );
}

// SavedPlans lists the plans kept; opening one values it at today's prices.
function SavedPlans({ canEdit, onOpen }: { canEdit: boolean; onOpen: (plan: AllocationSavedPlan, papers: Map<string, Instrument>) => void }) {
  const { t } = useTranslation();
  const plans = useSavedPlans();
  const remove = useDeletePlan();
  const papers = useInstrumentIndex();
  const list = plans.data ?? [];
  if (list.length === 0) return null;
  return (
    <div className="grid gap-1 border-t pt-3" data-testid="plan-saved">
      <p className="text-sm font-medium">{t("plan.saved")}</p>
      {list.map((p) => (
        <div key={p.id} className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 text-sm" data-testid="plan-saved-row">
          <span className="min-w-0 break-words">
            {p.name}
            <span className="text-xs text-muted-foreground"> · {t("plan.savedOn", { date: formatDate(p.saved_at.slice(0, 10)), count: p.operations.length })}</span>
          </span>
          <span className="flex gap-3">
            <Button variant="link" className="h-auto p-0 text-xs" onClick={() => onOpen(p, papers)}>
              {t("plan.open")}
            </Button>
            {canEdit && (
              <Button variant="link" className="h-auto p-0 text-xs" disabled={remove.isPending} onClick={() => remove.mutate(p.id)}>
                {t("plan.forget")}
              </Button>
            )}
          </span>
        </div>
      ))}
    </div>
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
  const [picked, setPicked] = useState<Instrument | null>(null);
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
          <InstrumentPicker
            value={picked}
            onChange={(i) => {
              setPicked(i);
              // A suggested sum is in the base currency, whatever the paper's.
              onChange({ ...draft, instrument: { id: i.id, name: i.name }, currency: draft.suggested ? draft.currency : i.currency });
            }}
          />
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
  const intermediaries = useIntermediaries();
  const members = useMembers();
  const title = (d: string) =>
    d === "kind"
      ? t("plan.cuts.kind")
      : d === "currency"
        ? t("plan.cuts.currency")
        : d === "country"
          ? t("plan.cuts.country")
          : d === "sector"
            ? t("plan.cuts.sector")
            : d === "broker"
              ? t("plan.cuts.broker")
              : t("plan.cuts.bank");
  const holder = (k: string) => (k === "-" ? t("structure.unknownHolder") : (intermediaries.data?.find((i) => i.id === k)?.name ?? k));
  const name = (d: string, k: string) =>
    d === "kind"
      ? kindName(t, k, c)
      : d === "currency"
        ? currencyName(t, k)
        : d === "country"
          ? k === "-" ? t("structure.noIsin") : countryName(k)
          : d === "sector"
            ? sectorName(t, k)
            : holder(k);
  const verdict = (v: string) =>
    v === "closer" ? t("plan.verdicts.closer") : v === "further" ? t("plan.verdicts.further") : v === "same" ? t("plan.verdicts.same") : t("plan.verdicts.none");
  const short = data.cuts.find((x) => x.dimension === "kind")?.rows.find((r) => r.key === "broker_cash" && r.after_minor < 0);
  const person = (id: string) => members.data?.find((m) => m.id === id)?.display_name ?? "";
  return (
    <div className="grid gap-3" data-testid="plan-result">
      {short && <p className="text-sm text-amber-700 dark:text-amber-400">{t("plan.short", { amount: formatMinorCompact(-short.after_minor, c) })}</p>}
      {data.uninsured.map((u) => (
        <p key={`${u.user_id}-${u.bank}`} className="text-sm text-amber-700 dark:text-amber-400" data-testid="plan-uninsured">
          {t("plan.uninsured", { person: person(u.user_id), bank: holder(u.bank), amount: formatMinorCompact(u.after_minor, c) })}
        </p>
      ))}
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
                    ` · ${cut.dimension === "kind" || cut.dimension === "currency" ? t("plan.target", { value: percent(r.target) }) : t("plan.ceiling", { value: percent(r.target) })}`}
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
