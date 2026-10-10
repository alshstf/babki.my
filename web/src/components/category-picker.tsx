import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { treeOf, type Category, type CategoryKind } from "@/api/categories";
import type { OperationType } from "@/api/operations";

const NONE = "none";

// The kind of category a row of this type takes, as the server rules
// (operation.categoryKind): money in earns, money out spends.
export function categoryKindOf(type: OperationType): CategoryKind {
  return type === "deposit" || type === "interest" ? "income" : "expense";
}

// A category's name with its parent's, «Транспорт › Такси», so a child reads
// on its own in a journal row; undefined for an id not in the list.
export function categoryLabel(categories: Category[], id: string | null | undefined): string | undefined {
  if (!id) return undefined;
  const found = categories.find((c) => c.id === id);
  if (!found) return undefined;
  const parent = found.parent_id ? categories.find((c) => c.id === found.parent_id) : undefined;
  return parent ? `${parent.name} › ${found.name}` : found.name;
}

// The categories a row may be filed under, in tree order with each child after
// its parent: the kind's active ones, and the row's own even when archived.
function choices(categories: Category[], kind: CategoryKind, current: string | null | undefined) {
  const keep = (c: Category) => !c.archived || c.id === current;
  return treeOf(categories, kind).flatMap((top) => [
    ...(keep(top) ? [{ category: top as Category, child: false }] : []),
    ...top.children.filter(keep).map((child) => ({ category: child, child: true })),
  ]);
}

// CategorySelect picks a row's category in a form, «без категории» first.
export function CategorySelect({
  id,
  categories,
  kind,
  value,
  onChange,
}: {
  id?: string;
  categories: Category[];
  kind: CategoryKind;
  value: string | null;
  onChange: (value: string | null) => void;
}) {
  const { t } = useTranslation();
  return (
    <Select value={value ?? NONE} onValueChange={(v) => onChange(v === NONE ? null : v)}>
      <SelectTrigger id={id} data-testid="category-select">
        <SelectValue />
      </SelectTrigger>
      <SelectContent className="max-h-80">
        <SelectItem value={NONE}>{t("categoryPicker.none")}</SelectItem>
        {choices(categories, kind, value).map(({ category, child }) => (
          <SelectItem key={category.id} value={category.id} className={child ? "pl-6" : undefined}>
            {category.name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

// CategoryChip shows a journal row's category; for someone who may change it,
// it opens a list to file the row under another one or take it out.
export function CategoryChip({
  categories,
  kind,
  value,
  editable,
  pending,
  onChange,
}: {
  categories: Category[];
  kind: CategoryKind;
  value: string | null;
  editable: boolean;
  pending?: boolean;
  onChange: (value: string | null) => void;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const label = categoryLabel(categories, value);
  // A word typed narrows the list to the categories it is part of, a child
  // also by its parent's name: «транс» finds the taxis.
  const wanted = query.trim().toLocaleLowerCase("ru");
  const shown = choices(categories, kind, value).filter(
    ({ category }) => !wanted || (categoryLabel(categories, category.id) ?? "").toLocaleLowerCase("ru").includes(wanted),
  );
  if (!editable) {
    return label ? (
      <span className="text-xs text-muted-foreground" data-testid="operation-category">
        {label}
      </span>
    ) : null;
  }
  const pick = (next: string | null) => {
    setOpen(false);
    if (next !== value) onChange(next);
  };
  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) setQuery("");
      }}
    >
      <PopoverTrigger asChild>
        <button
          type="button"
          disabled={pending}
          data-testid="operation-category"
          className={cn(
            "rounded-md border px-1.5 py-0.5 text-xs hover:bg-accent disabled:opacity-50",
            // Outlined, unlike the type's filled badge above it.
            label ? "border-border" : "border-dashed text-muted-foreground",
          )}
        >
          {label ?? t("categoryPicker.choose")}
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-64 p-1">
        <Input
          autoFocus
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={t("categoryPicker.search")}
          aria-label={t("categoryPicker.search")}
          className="mb-1 h-8"
        />
        <ul className="max-h-72 overflow-y-auto text-sm" role="listbox" aria-label={t("categoryPicker.title")}>
          {value && !wanted && (
            <li>
              <button
                type="button"
                role="option"
                aria-selected={false}
                className="w-full rounded px-2 py-1 text-left text-muted-foreground hover:bg-accent"
                onClick={() => pick(null)}
              >
                {t("categoryPicker.clear")}
              </button>
            </li>
          )}
          {shown.map(({ category, child }) => (
            // Found by a word, a child is named with its parent.
            <li key={category.id}>
              <button
                type="button"
                role="option"
                aria-selected={category.id === value}
                className={cn(
                  "w-full rounded px-2 py-1 text-left hover:bg-accent",
                  child && !wanted && "pl-6",
                  category.id === value && "font-medium",
                )}
                onClick={() => pick(category.id)}
              >
                {wanted ? categoryLabel(categories, category.id) : category.name}
              </button>
            </li>
          ))}
          {shown.length === 0 && <li className="px-2 py-1 text-muted-foreground">{t("categoryPicker.nothing")}</li>}
        </ul>
      </PopoverContent>
    </Popover>
  );
}
