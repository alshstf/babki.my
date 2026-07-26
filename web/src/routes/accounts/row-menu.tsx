import { useTranslation } from "react-i18next";
import { MoreHorizontal } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import type { AccountWithBalance } from "@/api/accounts";

export function RowMenu({
  account,
  onEdit,
  onBalance,
  onArchive,
}: {
  account: AccountWithBalance;
  onEdit: (account: AccountWithBalance) => void;
  onBalance: (account: AccountWithBalance) => void;
  onArchive: (account: AccountWithBalance) => void;
}) {
  const { t } = useTranslation();
  const archived = account.status === "archived";
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon" aria-label={t("common.actions")}>
          <MoreHorizontal className="size-4" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem onClick={() => onBalance(account)} disabled={archived}>
          {t("accounts.menu.balance")}
        </DropdownMenuItem>
        <DropdownMenuItem onClick={() => onEdit(account)}>
          {t("accounts.menu.edit")}
        </DropdownMenuItem>
        <DropdownMenuItem
          className="text-red-500"
          onClick={() => onArchive(account)}
          disabled={archived}
        >
          {t("accounts.menu.archive")}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
