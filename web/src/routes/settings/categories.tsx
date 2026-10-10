import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { MoreHorizontal } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useSession } from "@/api/session";
import {
  treeOf,
  useCategories,
  useCreateCategory,
  useDeleteCategory,
  useUpdateCategory,
  type Category,
  type CategoryKind,
  type CategoryNode,
} from "@/api/categories";
import { QueryGate } from "@/components/query-notice";
import { CategoryRules } from "./category-rules";
import { queryState } from "@/lib/query-state";

const KINDS: CategoryKind[] = ["expense", "income"];
const TOP = "top";
const MAX_NAME = 60;

// taken reports a name already used at that place: the server refuses it
// too, but the form says so before anything is sent.
function taken(categories: Category[], kind: CategoryKind, parentId: string | null, name: string, except?: string) {
  const wanted = name.trim().toLocaleLowerCase("ru");
  return categories.some(
    (c) => c.id !== except && c.kind === kind && c.parent_id === parentId && c.name.toLocaleLowerCase("ru") === wanted,
  );
}

// The edit dialog's subject: a new category (under a parent or on top) or an
// existing one to rename or move.
type Editing =
  | { mode: "create"; kind: CategoryKind; parentId: string | null }
  | { mode: "edit"; category: Category; hasChildren: boolean };

// CategoriesPage is where a family shapes its categories of money going out
// and coming in: a tree two levels deep per kind. Viewers read it; editors
// and the owner change it.
export function CategoriesPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const categories = useCategories();
  const update = useUpdateCategory();
  const remove = useDeleteCategory();
  const [showArchived, setShowArchived] = useState(false);
  const [editing, setEditing] = useState<Editing | null>(null);
  const list = categories.data ?? [];

  const state = queryState(categories);
  return (
    <div className="grid gap-6">
      <Link to="/settings" className="text-sm text-muted-foreground hover:underline">
        {t("categories.back")}
      </Link>
      <div className="grid gap-1">
        <h1 className="text-2xl font-bold">{t("categories.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("categories.hint")}</p>
      </div>
      <label className="flex items-center gap-2 text-sm">
        <Checkbox
          checked={showArchived}
          onCheckedChange={(v) => setShowArchived(v === true)}
          data-testid="categories-show-archived"
        />
        {t("categories.showArchived")}
      </label>
      {(update.isError || remove.isError) && (
        <Alert variant="destructive" data-testid="categories-error">
          <AlertDescription>
            {remove.isError ? t("categories.deleteFailed") : t("categories.saveFailed")}
          </AlertDescription>
        </Alert>
      )}
      <QueryGate state={state} />
      {state === "ready" && (
        <div className="grid gap-6 md:grid-cols-2">
          {KINDS.map((kind) => (
            <KindCard
              key={kind}
              kind={kind}
              tree={treeOf(list, kind)}
              all={list}
              canEdit={canEdit}
              showArchived={showArchived}
              onEdit={setEditing}
              onArchive={(c, archived) => {
                remove.reset();
                update.mutate({ id: c.id, body: { archived } });
              }}
              onDelete={(c) => {
                update.reset();
                remove.mutate(c.id);
              }}
            />
          ))}
        </div>
      )}
      {state === "ready" && <CategoryRules categories={list} canEdit={canEdit} />}
      {editing && (
        <CategoryDialog editing={editing} all={list} onClose={() => setEditing(null)} />
      )}
    </div>
  );
}

function KindCard({
  kind,
  tree,
  all,
  canEdit,
  showArchived,
  onEdit,
  onArchive,
  onDelete,
}: {
  kind: CategoryKind;
  tree: CategoryNode[];
  all: Category[];
  canEdit: boolean;
  showArchived: boolean;
  onEdit: (e: Editing) => void;
  onArchive: (c: Category, archived: boolean) => void;
  onDelete: (c: Category) => void;
}) {
  const { t } = useTranslation();
  const shown = (c: Category) => showArchived || !c.archived;
  return (
    <Card data-testid={`categories-${kind}`}>
      <CardHeader className="flex flex-row items-center justify-between">
        <CardTitle>{t(`categories.kinds.${kind}`)}</CardTitle>
        {canEdit && (
          <Button
            size="sm"
            variant="outline"
            onClick={() => onEdit({ mode: "create", kind, parentId: null })}
            data-testid={`categories-add-${kind}`}
          >
            {t("categories.add")}
          </Button>
        )}
      </CardHeader>
      <CardContent>
        <ul className="grid gap-1">
          {tree.filter(shown).map((top) => (
            <li key={top.id}>
              <Row
                category={top}
                canEdit={canEdit}
                onEdit={() => onEdit({ mode: "edit", category: top, hasChildren: top.children.length > 0 })}
                onAddChild={() => onEdit({ mode: "create", kind, parentId: top.id })}
                onArchive={onArchive}
                onDelete={onDelete}
              />
              {top.children.some(shown) && (
                <ul className="ml-6 grid gap-1 border-l pl-3">
                  {top.children.filter(shown).map((child) => (
                    <li key={child.id}>
                      <Row
                        category={child}
                        canEdit={canEdit}
                        onEdit={() => onEdit({ mode: "edit", category: child, hasChildren: false })}
                        onArchive={onArchive}
                        onDelete={onDelete}
                      />
                    </li>
                  ))}
                </ul>
              )}
            </li>
          ))}
        </ul>
        {all.every((c) => c.kind !== kind) && (
          <p className="text-sm text-muted-foreground">{t("categories.empty")}</p>
        )}
      </CardContent>
    </Card>
  );
}

function Row({
  category,
  canEdit,
  onEdit,
  onAddChild,
  onArchive,
  onDelete,
}: {
  category: Category;
  canEdit: boolean;
  onEdit: () => void;
  onAddChild?: () => void;
  onArchive: (c: Category, archived: boolean) => void;
  onDelete: (c: Category) => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="flex items-center justify-between gap-2 rounded-md px-2 py-0.5 hover:bg-accent" data-testid="category-row">
      <span className={category.archived ? "text-muted-foreground" : undefined}>
        {category.name}
        {category.archived && (
          <Badge variant="outline" className="ml-2">
            {t("categories.archived")}
          </Badge>
        )}
      </span>
      {canEdit && (
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label={t("categories.actions", { name: category.name })}>
              <MoreHorizontal className="size-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-auto">
            <DropdownMenuItem onSelect={onEdit}>{t("categories.edit")}</DropdownMenuItem>
            {onAddChild && !category.archived && (
              <DropdownMenuItem onSelect={onAddChild}>{t("categories.addChild")}</DropdownMenuItem>
            )}
            <DropdownMenuItem onSelect={() => onArchive(category, !category.archived)}>
              {category.archived ? t("categories.unarchive") : t("categories.archive")}
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => onDelete(category)}>{t("categories.delete")}</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      )}
    </div>
  );
}

// CategoryDialog names a new category or renames and moves one. A category
// with others under it stays on the top level (the tree is two deep).
function CategoryDialog({ editing, all, onClose }: { editing: Editing; all: Category[]; onClose: () => void }) {
  const { t } = useTranslation();
  const create = useCreateCategory();
  const update = useUpdateCategory();
  const kind = editing.mode === "create" ? editing.kind : editing.category.kind;
  const [name, setName] = useState(editing.mode === "edit" ? editing.category.name : "");
  const [place, setPlace] = useState<string>(
    (editing.mode === "create" ? editing.parentId : editing.category.parent_id) ?? TOP,
  );
  const self = editing.mode === "edit" ? editing.category.id : undefined;
  const parents = all.filter((c) => c.kind === kind && c.parent_id === null && c.id !== self && !c.archived);
  const canMove = editing.mode === "create" || !editing.hasChildren;
  const parentId = place === TOP ? null : place;
  const trimmed = name.trim();
  const isTaken = trimmed !== "" && taken(all, kind, parentId, trimmed, self);
  const valid = trimmed !== "" && trimmed.length <= MAX_NAME && !isTaken;
  const pending = create.isPending || update.isPending;
  const failed = create.isError || update.isError;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!valid) return;
    const done = { onSuccess: onClose };
    if (editing.mode === "create") {
      create.mutate({ kind, name: trimmed, parent_id: parentId }, done);
    } else {
      update.mutate({ id: editing.category.id, body: { name: trimmed, parent_id: parentId } }, done);
    }
  };

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" data-testid="category-dialog">
          <DialogHeader>
            <DialogTitle>
              {editing.mode === "create" ? t(`categories.newTitle.${kind}`) : t("categories.editTitle")}
            </DialogTitle>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="category-name">{t("categories.name")}</Label>
            <Input
              id="category-name"
              value={name}
              maxLength={MAX_NAME}
              onChange={(e) => setName(e.target.value)}
              autoFocus
            />
            {isTaken && <p className="text-sm text-destructive">{t("categories.nameTaken")}</p>}
          </div>
          {canMove ? (
            <div className="grid gap-2">
              <Label>{t("categories.place")}</Label>
              <Select value={place} onValueChange={setPlace}>
                <SelectTrigger aria-label={t("categories.place")}>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={TOP}>{t("categories.topLevel")}</SelectItem>
                  {parents.map((p) => (
                    <SelectItem key={p.id} value={p.id}>
                      {t("categories.under", { name: p.name })}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">{t("categories.staysOnTop")}</p>
          )}
          {failed && (
            <Alert variant="destructive">
              <AlertDescription>{t("categories.saveFailed")}</AlertDescription>
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
