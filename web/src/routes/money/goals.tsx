import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Progress } from "@/components/ui/progress";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { useAccounts } from "@/api/accounts";
import { useDeleteGoal, useGoals, useSaveGoal, type Goal } from "@/api/goals";
import { useSession } from "@/api/session";
import { formatDate } from "@/lib/dates";
import { formatMinorCompact, minorToInput, parseToMinor } from "@/lib/money";
import { hundredths, percent } from "@/lib/percent";
import { cn } from "@/lib/utils";

// The words for a goal's status; a switch with literal keys for
// scripts/check-i18n.mjs.
function statusName(t: (key: string) => string, status: Goal["status"]): string | null {
  switch (status) {
    case "done":
      return t("goals.status.done");
    case "ahead":
      return t("goals.status.ahead");
    case "on_track":
      return t("goals.status.onTrack");
    case "at_risk":
      return t("goals.status.atRisk");
    default:
      return null;
  }
}

// GoalsCard is the family's savings goals (decision Р-44): each against its
// sum and date — saved by the same reckoning as «Итого», the plan to the date,
// what it falls short by and what a month would do — and the accounts it
// stands on. Nothing for a viewer while there are none.
export function GoalsCard() {
  const { t } = useTranslation();
  const goals = useGoals();
  const accounts = useAccounts();
  const { data: session } = useSession();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const [editing, setEditing] = useState<{ goal?: Goal } | null>(null);
  const data = goals.data;
  if (!data || (data.goals.length === 0 && !canEdit)) return null;
  const c = data.base_currency;
  const accountName = (id: string) => accounts.data?.find((a) => a.id === id)?.name ?? t("goals.unknownAccount");
  const overdrawn = [...new Set(data.goals.flatMap((g) => g.overdrawn))];

  return (
    <Card data-testid="money-goals">
      <CardHeader className="grid gap-1">
        <CardTitle>{t("goals.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("goals.hint")}</p>
      </CardHeader>
      <CardContent className="grid grid-cols-1 gap-4">
        {data.goals.length === 0 && (
          <p className="text-sm text-muted-foreground" data-testid="goals-empty">
            {t("goals.empty")}
          </p>
        )}
        {data.goals.map((g) => (
          <GoalRow key={g.id} goal={g} currency={c} accountName={accountName} onEdit={canEdit ? () => setEditing({ goal: g }) : undefined} />
        ))}
        {overdrawn.length > 0 && (
          <Alert data-testid="goals-overdrawn">
            <AlertDescription>{t("goals.overdrawn", { accounts: overdrawn.map(accountName).join(", ") })}</AlertDescription>
          </Alert>
        )}
        {canEdit && (
          <Button type="button" size="sm" variant="outline" className="justify-self-start" onClick={() => setEditing({})}>
            {t("goals.add")}
          </Button>
        )}
      </CardContent>
      {editing && <GoalDialog goal={editing.goal} currency={c} onClose={() => setEditing(null)} />}
    </Card>
  );
}

function GoalRow({
  goal: g,
  currency: c,
  accountName,
  onEdit,
}: {
  goal: Goal;
  currency: string;
  accountName: (id: string) => string;
  onEdit?: () => void;
}) {
  const { t } = useTranslation();
  const status = statusName(t, g.status);
  const money = (minor: number) => formatMinorCompact(minor, c);
  return (
    <div className="grid gap-1.5" data-testid="goal">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <span className="min-w-0 font-medium break-words">
          {onEdit ? (
            <button type="button" className="text-left underline-offset-2 hover:underline" onClick={onEdit}>
              {g.name}
            </button>
          ) : (
            g.name
          )}
          {g.target_on && <span className="font-normal text-muted-foreground"> · {t("goals.by", { date: formatDate(g.target_on) })}</span>}
        </span>
        {status && (
          <Badge
            variant={g.status === "at_risk" ? "destructive" : "secondary"}
            className={cn(g.status === "done" || g.status === "ahead" ? "text-green-700 dark:text-green-400" : undefined)}
            data-testid="goal-status"
          >
            {status}
          </Badge>
        )}
      </div>
      <div className="text-sm tabular-nums" data-testid="goal-saved">
        {g.target_minor !== null
          ? t("goals.savedOf", { saved: money(g.saved_minor), target: money(g.target_minor), percent: g.percent })
          : t("goals.savedOnly", { saved: money(g.saved_minor) })}
      </div>
      {g.target_minor !== null && (
        <Progress value={g.percent} className={cn(g.status === "at_risk" && "[&>*]:bg-amber-500", g.status === "done" && "[&>*]:bg-green-600")} />
      )}
      <Plan goal={g} money={money} />
      <ul className="grid gap-0.5 text-xs text-muted-foreground" data-testid="goal-funds">
        {g.funds.map((f) => (
          <li key={f.account_id}>
            {f.amount_minor === null
              ? t("goals.fundWhole", { account: accountName(f.account_id), value: money(f.value_minor) })
              : f.value_minor < f.amount_minor
                ? t("goals.fundPartShort", { account: accountName(f.account_id), amount: money(f.amount_minor), value: money(f.value_minor) })
                : t("goals.fundPart", { account: accountName(f.account_id), amount: money(f.amount_minor) })}
          </li>
        ))}
        {g.funds.length === 0 && <li>{t("goals.noFunds")}</li>}
        {Number(g.yield_percent) !== 0 && <li>{t("goals.yield", { percent: percent(g.yield_percent) })}</li>}
      </ul>
    </div>
  );
}

// Plan is what the contribution makes of the goal: by the date, or when it
// gets there without one; and, short of it, what would do.
function Plan({ goal: g, money }: { goal: Goal; money: (minor: number) => string }) {
  const { t } = useTranslation();
  if (g.status === "done" || g.target_minor === null) return null;
  const lines: { text: string; warn?: boolean }[] = [];
  if (g.target_on && g.projected_minor !== null) {
    if (g.monthly_minor !== null && g.months > 0) {
      lines.push({ text: t("goals.planMonthly", { months: g.months, monthly: money(g.monthly_minor), projected: money(g.projected_minor) }) });
    } else if (g.months > 0) {
      lines.push({ text: t("goals.planNoMonthly", { projected: money(g.projected_minor) }) });
    }
    if (g.status === "at_risk" && g.short_minor !== null && g.needed_minor !== null) {
      const fix = g.months > 0 ? t("goals.fixMonthly", { needed: money(g.needed_minor) }) : t("goals.fixPast");
      const later = g.reach_on ? t("goals.fixLater", { date: formatDate(g.reach_on) }) : "";
      lines.push({ text: `${t("goals.short", { short: money(g.short_minor) })} ${fix}${later}.`, warn: true });
    }
  } else if (g.reach_on && g.monthly_minor !== null) {
    lines.push({ text: t("goals.reachOn", { monthly: money(g.monthly_minor), date: formatDate(g.reach_on) }) });
  } else if (!g.target_on && g.monthly_minor === null) {
    lines.push({ text: t("goals.noPlan") });
  }
  if (lines.length === 0) return null;
  return (
    <div className="grid gap-0.5 text-sm" data-testid="goal-plan">
      {lines.map((l) => (
        <p key={l.text} className={cn(l.warn ? "text-amber-700 dark:text-amber-400" : "text-muted-foreground")}>
          {l.text}
        </p>
      ))}
    </div>
  );
}

type FundDraft = { account: string; whole: boolean; amount: string };

// GoalDialog sets a goal down: its name, sum, date, contribution and yield —
// all but the name optional — and the accounts it stands on, whole or a sum
// out of each; or takes it away.
function GoalDialog({ goal, currency, onClose }: { goal?: Goal; currency: string; onClose: () => void }) {
  const { t } = useTranslation();
  const accounts = useAccounts();
  const save = useSaveGoal();
  const remove = useDeleteGoal();
  const [name, setName] = useState(goal?.name ?? "");
  const [target, setTarget] = useState(goal?.target_minor != null ? minorToInput(goal.target_minor) : "");
  const [date, setDate] = useState(goal?.target_on ?? "");
  const [monthly, setMonthly] = useState(goal?.monthly_minor != null ? minorToInput(goal.monthly_minor) : "");
  const [yieldText, setYieldText] = useState(goal ? Number(goal.yield_percent).toLocaleString("ru-RU", { maximumFractionDigits: 2 }) : "0");
  const [funds, setFunds] = useState<FundDraft[]>(
    goal?.funds.map((f) => ({ account: f.account_id, whole: f.amount_minor === null, amount: f.amount_minor !== null ? minorToInput(f.amount_minor) : "" })) ?? [],
  );
  // Debts stand for no goal; an archived account is counted nowhere.
  const eligible = (accounts.data ?? []).filter((a) => a.status === "active" && a.type !== "loan" && a.type !== "credit_card");

  const sum = (s: string) => (s.trim() === "" ? null : parseToMinor(s));
  const targetMinor = sum(target);
  const monthlyMinor = sum(monthly);
  const yieldHundredths = hundredths(yieldText === "" ? "0" : yieldText);
  const fundMinor = funds.map((f) => (f.whole ? null : parseToMinor(f.amount)));
  const valid =
    name.trim() !== "" &&
    name.trim().length <= 100 &&
    (target.trim() === "" || (targetMinor !== null && targetMinor > 0)) &&
    (monthly.trim() === "" || (monthlyMinor !== null && monthlyMinor > 0)) &&
    yieldHundredths !== null &&
    yieldHundredths < 10_000 &&
    funds.every((f, i) => f.account !== "" && (f.whole || (fundMinor[i] !== null && fundMinor[i]! > 0))) &&
    new Set(funds.map((f) => f.account)).size === funds.length;

  const submit = () => {
    if (!valid) return;
    save.mutate(
      {
        id: goal?.id,
        goal: {
          name: name.trim(),
          target_minor: targetMinor,
          target_on: date === "" ? null : date,
          monthly_minor: monthlyMinor,
          yield_percent: (yieldText === "" ? "0" : yieldText).trim().replace(",", "."),
          funds: funds.map((f, i) => ({ account_id: f.account, amount_minor: f.whole ? null : fundMinor[i] })),
        },
      },
      { onSuccess: onClose },
    );
  };
  const setFund = (i: number, patch: Partial<FundDraft>) => setFunds(funds.map((f, j) => (j === i ? { ...f, ...patch } : f)));

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-lg" data-testid="goal-dialog">
        <DialogHeader>
          <DialogTitle>{goal ? t("goals.editTitle", { name: goal.name }) : t("goals.addTitle")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1">
            <Label htmlFor="goal-name">{t("goals.name")}</Label>
            <Input id="goal-name" maxLength={100} value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="grid gap-1">
              <Label htmlFor="goal-target">{t("goals.target", { currency })}</Label>
              <Input id="goal-target" inputMode="decimal" value={target} onChange={(e) => setTarget(e.target.value)} />
            </div>
            <div className="grid gap-1">
              <Label htmlFor="goal-date">{t("goals.date")}</Label>
              <Input id="goal-date" type="date" value={date} onChange={(e) => setDate(e.target.value)} />
            </div>
            <div className="grid gap-1">
              <Label htmlFor="goal-monthly">{t("goals.monthly", { currency })}</Label>
              <Input id="goal-monthly" inputMode="decimal" value={monthly} onChange={(e) => setMonthly(e.target.value)} />
            </div>
            <div className="grid gap-1">
              <Label htmlFor="goal-yield">{t("goals.yieldLabel")}</Label>
              <Input id="goal-yield" inputMode="decimal" value={yieldText} onChange={(e) => setYieldText(e.target.value)} />
            </div>
          </div>
          <p className="text-xs text-muted-foreground">{t("goals.optionalHint")}</p>
          <p className="text-xs text-muted-foreground">{t("goals.yieldHint")}</p>
          <div className="grid gap-2">
            <Label>{t("goals.funds")}</Label>
            {funds.map((f, i) => (
              <div key={i} className="grid grid-cols-[1fr_auto] gap-2 sm:grid-cols-[1fr_10.5rem_7rem_auto]" data-testid="goal-fund">
                <Select value={f.account} onValueChange={(v) => setFund(i, { account: v })}>
                  <SelectTrigger className="w-full min-w-0" aria-label={t("goals.account")}>
                    <SelectValue placeholder={t("goals.pickAccount")} />
                  </SelectTrigger>
                  <SelectContent>
                    {eligible.map((a) => (
                      <SelectItem key={a.id} value={a.id} disabled={a.id !== f.account && funds.some((o) => o.account === a.id)}>
                        {a.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <Button type="button" variant="ghost" size="sm" className="sm:order-last" aria-label={t("goals.removeFund")} onClick={() => setFunds(funds.filter((_, j) => j !== i))}>
                  ×
                </Button>
                <Select value={f.whole ? "whole" : "part"} onValueChange={(v) => setFund(i, { whole: v === "whole" })}>
                  <SelectTrigger className="w-full" aria-label={t("goals.howMuch")}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="whole">{t("goals.whole")}</SelectItem>
                    <SelectItem value="part">{t("goals.part")}</SelectItem>
                  </SelectContent>
                </Select>
                {!f.whole && (
                  <Input inputMode="decimal" aria-label={t("goals.partAmount", { currency })} placeholder={currency} value={f.amount} onChange={(e) => setFund(i, { amount: e.target.value })} />
                )}
              </div>
            ))}
            <Button type="button" size="sm" variant="ghost" className="justify-self-start" onClick={() => setFunds([...funds, { account: "", whole: true, amount: "" }])}>
              {t("goals.addFund")}
            </Button>
            <p className="text-xs text-muted-foreground">{t("goals.fundsHint")}</p>
          </div>
          {(save.isError || remove.isError) && (
            <Alert variant="destructive">
              <AlertDescription>{t("goals.saveFailed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter className="gap-2">
          {goal && (
            <Button
              type="button"
              variant="ghost"
              className="mr-auto"
              disabled={remove.isPending || save.isPending}
              onClick={() => remove.mutate(goal.id, { onSuccess: onClose })}
            >
              {t("goals.remove")}
            </Button>
          )}
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || save.isPending} onClick={submit}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
