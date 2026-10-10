import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Progress } from "@/components/ui/progress";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { CategorySelect, categoryLabel } from "@/components/category-picker";
import { useBudget, useSetBudgetLimit, type BudgetLine } from "@/api/budget";
import { useCategories } from "@/api/categories";
import { useSession } from "@/api/session";
import { localToday } from "@/lib/dates";
import { formatMinor, minorToInput, parseToMinor } from "@/lib/money";
import { cn } from "@/lib/utils";

// From this share of a limit spent the line warns.
const NEAR = 0.9;

// shift is the month n months from m, both YYYY-MM.
function shift(m: string, n: number): string {
  const d = new Date(Date.UTC(Number(m.slice(0, 4)), Number(m.slice(5, 7)) - 1 + n, 1));
  return d.toISOString().slice(0, 7);
}

// monthName is «Сентябрь 2026».
function monthName(m: string): string {
  const name = new Date(`${m}-01T00:00:00`).toLocaleDateString("ru-RU", { month: "long", year: "numeric" }).replace(/\s*г\.?$/, "");
  return name.charAt(0).toUpperCase() + name.slice(1);
}

// BudgetCard is the month's budget (decision Р-25): the limited categories'
// plan and fact, what a копилка brought from the months before, and the rest
// of the spending in one line. Nothing for a viewer while no limit is set.
export function BudgetCard() {
  const { t } = useTranslation();
  const [month, setMonth] = useState(() => localToday().slice(0, 7));
  const budget = useBudget(month);
  const categories = useCategories();
  const { data: session } = useSession();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const [editing, setEditing] = useState<{ line?: BudgetLine } | null>(null);
  const b = budget.data;
  if (!b || (b.lines.length === 0 && !canEdit)) return null;
  const c = b.base_currency;
  const cats = categories.data ?? [];

  return (
    <Card data-testid="money-budget">
      <CardHeader className="flex flex-row flex-wrap items-start justify-between gap-2">
        <div className="grid gap-1">
          <CardTitle>{t("budget.title")}</CardTitle>
          <p className="text-sm text-muted-foreground">{t("budget.hint")}</p>
        </div>
        <div className="flex items-center gap-1">
          <Button type="button" size="sm" variant="ghost" aria-label={t("budget.previous")} onClick={() => setMonth(shift(month, -1))}>
            ‹
          </Button>
          <span className="min-w-32 text-center text-sm font-medium" data-testid="budget-month">
            {monthName(month)}
          </span>
          <Button type="button" size="sm" variant="ghost" aria-label={t("budget.next")} onClick={() => setMonth(shift(month, 1))}>
            ›
          </Button>
        </div>
      </CardHeader>
      <CardContent className="grid grid-cols-1 gap-3">
        {b.lines.length === 0 ? (
          <p className="text-sm text-muted-foreground" data-testid="budget-empty">
            {t("budget.empty")}
          </p>
        ) : (
          <>
            <div className="grid grid-cols-3 gap-2" data-testid="budget-sums">
              <div>
                <div className="text-sm text-muted-foreground">{t("budget.planned")}</div>
                <div className="text-lg font-semibold tabular-nums">{formatMinor(b.planned_minor, c)}</div>
              </div>
              <div>
                <div className="text-sm text-muted-foreground">{t("budget.spent")}</div>
                <div className="text-lg font-semibold tabular-nums">{formatMinor(b.spent_minor, c)}</div>
              </div>
              <div>
                <div className="text-sm text-muted-foreground">{t("budget.left")}</div>
                <div className={cn("text-lg font-semibold tabular-nums", b.left_minor < 0 && "text-red-700 dark:text-red-400")}>
                  {formatMinor(b.left_minor, c)}
                </div>
              </div>
            </div>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("budget.category")}</TableHead>
                  <TableHead className="hidden text-right sm:table-cell">{t("budget.limit")}</TableHead>
                  <TableHead className="text-right">{t("budget.spent")}</TableHead>
                  <TableHead className="text-right">{t("budget.left")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {b.lines.map((l) => {
                  const planned = l.limit_minor + l.carried_minor;
                  const share = planned > 0 ? l.spent_minor / planned : l.spent_minor > 0 ? 1 : 0;
                  const name = categoryLabel(cats, l.category_id) ?? "?";
                  return (
                    <TableRow key={l.category_id} data-testid="budget-line">
                      <TableCell className="whitespace-normal">
                        {canEdit ? (
                          <button type="button" className="text-left underline-offset-2 hover:underline" onClick={() => setEditing({ line: l })}>
                            {name}
                          </button>
                        ) : (
                          name
                        )}
                        {l.rollover && (
                          <span className="text-xs text-muted-foreground">
                            {" · "}
                            {l.carried_minor > 0 ? t("budget.kopilkaCarried", { amount: formatMinor(l.carried_minor, c) }) : t("budget.kopilka")}
                          </span>
                        )}
                        <Progress
                          value={Math.min(share, 1) * 100}
                          className={cn("mt-1", share > 1 ? "[&>*]:bg-red-600" : share >= NEAR && "[&>*]:bg-amber-500")}
                        />
                      </TableCell>
                      <TableCell className="hidden text-right tabular-nums sm:table-cell">{formatMinor(l.limit_minor, c)}</TableCell>
                      <TableCell className="text-right tabular-nums">{formatMinor(l.spent_minor, c)}</TableCell>
                      <TableCell
                        className={cn(
                          "text-right tabular-nums",
                          l.left_minor < 0 ? "text-red-700 dark:text-red-400" : share >= NEAR && "text-amber-700 dark:text-amber-400",
                        )}
                      >
                        {formatMinor(l.left_minor, c)}
                      </TableCell>
                    </TableRow>
                  );
                })}
                <TableRow data-testid="budget-unlimited">
                  <TableCell className="text-muted-foreground">{t("budget.unlimited")}</TableCell>
                  <TableCell className="hidden sm:table-cell" />
                  <TableCell className="text-right tabular-nums text-muted-foreground">{formatMinor(b.unlimited_minor, c)}</TableCell>
                  <TableCell />
                </TableRow>
              </TableBody>
            </Table>
          </>
        )}
        {b.missing_rates.length > 0 && (
          <Alert>
            <AlertDescription>{t("budget.missingRates", { list: b.missing_rates.join(", ") })}</AlertDescription>
          </Alert>
        )}
        {canEdit && (
          <Button type="button" size="sm" variant="outline" className="justify-self-start" onClick={() => setEditing({})}>
            {t("budget.add")}
          </Button>
        )}
      </CardContent>
      {editing && <LimitDialog month={month} currency={c} line={editing.line} onClose={() => setEditing(null)} />}
    </Card>
  );
}

// LimitDialog states a category's limit from the shown month on: its amount
// and a копилка; or takes a line's limit off from the month.
function LimitDialog({ month, currency, line, onClose }: { month: string; currency: string; line?: BudgetLine; onClose: () => void }) {
  const { t } = useTranslation();
  const categories = useCategories();
  const save = useSetBudgetLimit();
  const [categoryId, setCategoryId] = useState<string | null>(line?.category_id ?? null);
  const [amount, setAmount] = useState(line ? minorToInput(line.limit_minor) : "");
  const [rollover, setRollover] = useState(line?.rollover ?? false);
  const minor = parseToMinor(amount.trim() === "" ? "0" : amount);
  const valid = categoryId !== null && minor !== null && minor >= 0 && (minor > 0 || rollover);
  const submit = (amountMinor: number, keep: boolean) => {
    if (!categoryId) return;
    save.mutate({ category_id: categoryId, from_month: month, amount_minor: amountMinor, rollover: keep }, { onSuccess: onClose });
  };
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-md" data-testid="budget-limit-dialog">
        <DialogHeader>
          <DialogTitle>{line ? t("budget.editTitle", { name: categoryLabel(categories.data ?? [], line.category_id) ?? "" }) : t("budget.addTitle")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          {!line && (
            <div className="grid gap-1">
              <Label htmlFor="budget-category">{t("budget.category")}</Label>
              <CategorySelect id="budget-category" categories={categories.data ?? []} kind="expense" value={categoryId} onChange={setCategoryId} />
            </div>
          )}
          <div className="grid gap-1">
            <Label htmlFor="budget-amount">{t("budget.amount", { currency })}</Label>
            <Input id="budget-amount" inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} />
            <p className="text-xs text-muted-foreground">{t("budget.fromMonth", { month: monthName(month) })}</p>
          </div>
          <div className="flex items-start gap-2">
            <Checkbox id="budget-rollover" checked={rollover} onCheckedChange={(v) => setRollover(v === true)} className="mt-0.5" />
            <div className="grid gap-0.5">
              <Label htmlFor="budget-rollover" className="font-normal">
                {t("budget.rollover")}
              </Label>
              <p className="text-xs text-muted-foreground">{t("budget.rolloverHint")}</p>
            </div>
          </div>
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("budget.saveFailed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter className="gap-2">
          {line && (
            <Button type="button" variant="ghost" className="mr-auto" disabled={save.isPending} onClick={() => submit(0, false)}>
              {t("budget.remove")}
            </Button>
          )}
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || save.isPending} onClick={() => minor !== null && submit(minor, rollover)}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
