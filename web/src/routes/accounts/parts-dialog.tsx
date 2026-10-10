import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { CategorySelect, categoryKindOf } from "@/components/category-picker";
import { useCategories } from "@/api/categories";
import { useSetOperationParts, type Operation } from "@/api/operations";
import { formatMinor, minorToInput, parseToMinor } from "@/lib/money";
import { cn } from "@/lib/utils";

interface Row {
  categoryId: string | null;
  amount: string;
}

// PartsDialog splits a row across categories (decision Р-36): the first line
// is the row's own category and takes whatever the others leave; the others
// are typed. A split row can be made one category's again.
export function PartsDialog({ operation, onClose }: { operation: Operation; onClose: () => void }) {
  const { t } = useTranslation();
  const categories = useCategories();
  const save = useSetOperationParts();
  const total = Math.abs(operation.amount_minor);
  const existing = operation.parts ?? [];
  const [main, setMain] = useState<string | null>(existing[0]?.category_id ?? operation.category_id ?? null);
  const [rows, setRows] = useState<Row[]>(() =>
    existing.length > 0
      ? existing.slice(1).map((p) => ({ categoryId: p.category_id, amount: minorToInput(p.amount_minor) }))
      : [{ categoryId: null, amount: "" }],
  );
  const amounts = rows.map((r) => parseToMinor(r.amount.trim() === "" ? "0" : r.amount));
  const others = amounts.reduce<number>((sum, v) => sum + (v ?? 0), 0);
  const rest = total - others;
  const kind = categoryKindOf(operation.type);
  const valid =
    main !== null && rest > 0 && rows.length > 0 &&
    rows.every((r, i) => r.categoryId !== null && amounts[i] !== null && (amounts[i] ?? 0) > 0);
  const set = (i: number, patch: Partial<Row>) => setRows((prev) => prev.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const submit = () => {
    if (!valid || main === null) return;
    const parts = [
      { category_id: main, amount_minor: rest },
      ...rows.map((r, i) => ({ category_id: r.categoryId as string, amount_minor: amounts[i] as number })),
    ];
    save.mutate({ operationId: operation.id, parts }, { onSuccess: onClose });
  };
  const c = operation.currency;
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-md" data-testid="parts-dialog">
        <DialogHeader>
          <DialogTitle>{t("parts.title", { amount: formatMinor(total, c) })}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <p className="text-xs text-muted-foreground">{t("parts.hint")}</p>
          <div className="grid gap-1">
            <Label htmlFor="parts-main">{t("parts.main")}</Label>
            <div className="flex items-center gap-2">
              <div className="flex-1">
                <CategorySelect id="parts-main" categories={categories.data ?? []} kind={kind} value={main} onChange={setMain} />
              </div>
              <span className={cn("w-28 text-right text-sm tabular-nums", rest <= 0 && "text-red-700 dark:text-red-400")} data-testid="parts-rest">
                {formatMinor(rest, c)}
              </span>
            </div>
          </div>
          {rows.map((r, i) => (
            <div key={i} className="grid gap-1" data-testid="parts-row">
              <Label htmlFor={`parts-category-${i}`}>{t("parts.part", { n: i + 2 })}</Label>
              <div className="flex items-center gap-2">
                <div className="flex-1">
                  <CategorySelect
                    id={`parts-category-${i}`}
                    categories={categories.data ?? []}
                    kind={kind}
                    value={r.categoryId}
                    onChange={(v) => set(i, { categoryId: v })}
                  />
                </div>
                <Input
                  id={`parts-amount-${i}`}
                  aria-label={t("parts.amount", { n: i + 2 })}
                  className="w-28 text-right"
                  inputMode="decimal"
                  value={r.amount}
                  onChange={(e) => set(i, { amount: e.target.value })}
                />
                {rows.length > 1 && (
                  <Button type="button" size="sm" variant="ghost" aria-label={t("parts.remove")} onClick={() => setRows(rows.filter((_, j) => j !== i))}>
                    ×
                  </Button>
                )}
              </div>
            </div>
          ))}
          <Button type="button" size="sm" variant="outline" className="justify-self-start" onClick={() => setRows([...rows, { categoryId: null, amount: "" }])}>
            {t("parts.add")}
          </Button>
          {rest <= 0 && <p className="text-xs text-red-700 dark:text-red-400">{t("parts.tooMuch")}</p>}
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("parts.failed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter className="gap-2">
          {existing.length > 0 && (
            <Button
              type="button"
              variant="ghost"
              className="mr-auto"
              disabled={save.isPending}
              onClick={() => save.mutate({ operationId: operation.id, parts: null }, { onSuccess: onClose })}
            >
              {t("parts.merge")}
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
