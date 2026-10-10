import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import type { MemberInfo } from "@/api/members";

const OWNER = "owner";

// MemberSelect says whose a row is in a form: the account's owner (or the
// family, on a shared account) unless a member is picked.
export function MemberSelect({
  id,
  members,
  value,
  onChange,
  shared,
}: {
  id?: string;
  members: MemberInfo[];
  value: string | null;
  onChange: (value: string | null) => void;
  // A shared account has no owner: a row naming nobody is the family's.
  shared?: boolean;
}) {
  const { t } = useTranslation();
  return (
    <Select value={value ?? OWNER} onValueChange={(v) => onChange(v === OWNER ? null : v)}>
      <SelectTrigger id={id} data-testid="member-select">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={OWNER}>{shared ? t("memberPicker.family") : t("memberPicker.owner")}</SelectItem>
        {members.map((m) => (
          <SelectItem key={m.id} value={m.id}>
            {m.display_name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

// MemberChip shows whose a journal row is when it names someone; for someone
// who may change it, it opens the family's members to pick from.
export function MemberChip({
  members,
  value,
  editable,
  pending,
  onChange,
  shared,
}: {
  members: MemberInfo[];
  value: string | null;
  editable: boolean;
  pending?: boolean;
  onChange: (value: string | null) => void;
  shared?: boolean;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const name = members.find((m) => m.id === value)?.display_name;
  if (!editable) {
    return name ? (
      <span className="text-xs text-muted-foreground" data-testid="operation-member">
        {name}
      </span>
    ) : null;
  }
  const pick = (next: string | null) => {
    setOpen(false);
    if (next !== value) onChange(next);
  };
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          disabled={pending}
          data-testid="operation-member"
          className={cn(
            "rounded-md border px-1.5 py-0.5 text-xs hover:bg-accent disabled:opacity-50",
            // Unnamed, it stays out of the way until wanted.
            name ? "border-border" : "border-transparent text-muted-foreground/70 hover:border-dashed hover:border-border",
          )}
        >
          {name ?? t("memberPicker.whose")}
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-56 p-1">
        <ul className="text-sm" role="listbox" aria-label={t("memberPicker.title")}>
          {[{ id: null as string | null, label: shared ? t("memberPicker.family") : t("memberPicker.owner") }, ...members.map((m) => ({ id: m.id as string | null, label: m.display_name }))].map(
            (item) => (
              <li key={item.id ?? OWNER}>
                <button
                  type="button"
                  role="option"
                  aria-selected={item.id === value}
                  className={cn("w-full rounded px-2 py-1 text-left hover:bg-accent", item.id === value && "font-medium")}
                  onClick={() => pick(item.id)}
                >
                  {item.label}
                </button>
              </li>
            ),
          )}
        </ul>
      </PopoverContent>
    </Popover>
  );
}
