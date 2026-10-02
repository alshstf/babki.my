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
  onRestore,
}: {
  account: AccountWithBalance;
  onEdit: (account: AccountWithBalance) => void;
  onBalance: (account: AccountWithBalance) => void;
  onArchive: (account: AccountWithBalance) => void;
  onRestore: (account: AccountWithBalance) => void;
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
        {/* An archived account takes no entries until it is brought back, so
            the way back sits where the way in was. */}
        {archived ? (
          <DropdownMenuItem onClick={() => onRestore(account)}>
            {t("accounts.menu.restore")}
          </DropdownMenuItem>
        ) : (
          <DropdownMenuItem className="text-red-500" onClick={() => onArchive(account)}>
            {t("accounts.menu.archive")}
          </DropdownMenuItem>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
