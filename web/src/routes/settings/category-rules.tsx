import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { ArrowDown, ArrowUp, Pencil, Trash2 } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  treeOf,
  useCategoryRules,
  useCreateCategoryRule,
  useDeleteCategoryRule,
  useReorderCategoryRules,
  useUpdateCategoryRule,
  type Category,
  type CategoryRule,
  type CategoryRuleField,
} from "@/api/categories";
import { categoryLabel } from "@/components/category-picker";
import { QueryGate } from "@/components/query-notice";
import { queryState } from "@/lib/query-state";

const FIELDS: CategoryRuleField[] = ["counterparty", "note", "any"];
const MAX_PATTERN = 200;

// CategoryRules lists the family's filing rules — «when the counterparty holds
// this text, file the row under that category» — in the order they are tried.
export function CategoryRules({ categories, canEdit }: { categories: Category[]; canEdit: boolean }) {
  const { t } = useTranslation();
  const rules = useCategoryRules();
  const reorder = useReorderCategoryRules();
  const remove = useDeleteCategoryRule();
  const [editing, setEditing] = useState<CategoryRule | "new" | null>(null);
  const list = rules.data ?? [];
  const state = queryState(rules);

  const move = (index: number, by: -1 | 1) => {
    const ids = list.map((r) => r.id);
    [ids[index], ids[index + by]] = [ids[index + by], ids[index]];
    reorder.mutate(ids);
  };

  return (
    <Card data-testid="category-rules">
      <CardHeader className="flex flex-row items-start justify-between gap-4">
        <div className="grid gap-1">
          <CardTitle>{t("categoryRules.title")}</CardTitle>
          <p className="text-sm text-muted-foreground">{t("categoryRules.hint")}</p>
        </div>
        {canEdit && (
          <Button size="sm" variant="outline" onClick={() => setEditing("new")} data-testid="category-rules-add">
            {t("categoryRules.add")}
          </Button>
        )}
      </CardHeader>
      <CardContent className="grid gap-2">
        <QueryGate state={state} />
        {(reorder.isError || remove.isError) && (
          <Alert variant="destructive">
            <AlertDescription>{t("categoryRules.failed")}</AlertDescription>
          </Alert>
        )}
        {state === "ready" && list.length === 0 && (
          <p className="text-sm text-muted-foreground">{t("categoryRules.empty")}</p>
        )}
        <ol className="grid gap-1">
          {list.map((rule, i) => (
            <li
              key={rule.id}
              className="flex items-center justify-between gap-2 rounded-md px-2 py-1 hover:bg-accent"
              data-testid="category-rule"
            >
              <span className="text-sm">
                {t(`categoryRules.fields.${rule.field}`)} {t("categoryRules.holds")} «{rule.pattern}» →{" "}
                <span className="font-medium">
                  {categoryLabel(categories, rule.category_id) ?? t("categoryRules.unknownCategory")}
                </span>
              </span>
              {canEdit && (
                <span className="flex shrink-0">
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    disabled={i === 0 || reorder.isPending}
                    aria-label={t("categoryRules.up")}
                    onClick={() => move(i, -1)}
                  >
                    <ArrowUp className="size-4" />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    disabled={i === list.length - 1 || reorder.isPending}
                    aria-label={t("categoryRules.down")}
                    onClick={() => move(i, 1)}
                  >
                    <ArrowDown className="size-4" />
                  </Button>
                  <Button variant="ghost" size="icon-sm" aria-label={t("categoryRules.edit")} onClick={() => setEditing(rule)}>
                    <Pencil className="size-4" />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={t("categoryRules.delete")}
                    onClick={() => remove.mutate(rule.id)}
                  >
                    <Trash2 className="size-4" />
                  </Button>
                </span>
              )}
            </li>
          ))}
        </ol>
      </CardContent>
      {editing && (
        <RuleDialog
          rule={editing === "new" ? undefined : editing}
          categories={categories}
          onClose={() => setEditing(null)}
        />
      )}
    </Card>
  );
}

function RuleDialog({
  rule,
  categories,
  onClose,
}: {
  rule?: CategoryRule;
  categories: Category[];
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const create = useCreateCategoryRule();
  const update = useUpdateCategoryRule();
  const [field, setField] = useState<CategoryRuleField>(rule?.field ?? "counterparty");
  const [pattern, setPattern] = useState(rule?.pattern ?? "");
  const [categoryId, setCategoryId] = useState(rule?.category_id ?? "");
  const valid = pattern.trim() !== "" && categoryId !== "";
  const pending = create.isPending || update.isPending;
  // Both kinds, each child under its parent, active ones and the rule's own.
  const groups = (["expense", "income"] as const).map((kind) => ({
    kind,
    options: treeOf(categories, kind).flatMap((top) => [
      { category: top as Category, child: false },
      ...top.children.map((child) => ({ category: child, child: true })),
    ]).filter(({ category }) => !category.archived || category.id === rule?.category_id),
  }));

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!valid) return;
    const body = { field, pattern: pattern.trim(), category_id: categoryId };
    const done = { onSuccess: onClose };
    if (rule) update.mutate({ id: rule.id, body }, done);
    else create.mutate(body, done);
  };

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" data-testid="category-rule-dialog">
          <DialogHeader>
            <DialogTitle>{rule ? t("categoryRules.editTitle") : t("categoryRules.newTitle")}</DialogTitle>
          </DialogHeader>
          <div className="grid gap-2">
            <Label>{t("categoryRules.field")}</Label>
            <Select value={field} onValueChange={(v) => setField(v as CategoryRuleField)}>
              <SelectTrigger aria-label={t("categoryRules.field")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {FIELDS.map((f) => (
                  <SelectItem key={f} value={f}>
                    {t(`categoryRules.fields.${f}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="rule-pattern">{t("categoryRules.pattern")}</Label>
            <Input
              id="rule-pattern"
              value={pattern}
              maxLength={MAX_PATTERN}
              onChange={(e) => setPattern(e.target.value)}
              autoFocus
            />
            <p className="text-xs text-muted-foreground">{t("categoryRules.patternHint")}</p>
          </div>
          <div className="grid gap-2">
            <Label>{t("categoryRules.category")}</Label>
            <Select value={categoryId} onValueChange={setCategoryId}>
              <SelectTrigger aria-label={t("categoryRules.category")}>
                <SelectValue placeholder={t("categoryRules.choose")} />
              </SelectTrigger>
              <SelectContent className="max-h-80">
                {groups.map((group) => (
                  <SelectGroup key={group.kind}>
                    <SelectLabel>{t(`categories.kinds.${group.kind}`)}</SelectLabel>
                    {group.options.map(({ category, child }) => (
                      <SelectItem key={category.id} value={category.id} className={child ? "pl-6" : undefined}>
                        {category.name}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                ))}
              </SelectContent>
            </Select>
          </div>
          {(create.isError || update.isError) && (
            <Alert variant="destructive">
              <AlertDescription>{t("categoryRules.failed")}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("categories.cancel")}
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {t("categories.save")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
