import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { formatDate } from "@/lib/dates";
import { resolveDisplayAmount } from "@/lib/display-amount";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import { MoneyCell } from "@/components/money-cell";
import type { AccountWithBalance } from "@/api/accounts";

export function AccountsTable({
  accounts,
  mode,
  baseCurrency,
  onRowAction,
}: {
  accounts: AccountWithBalance[];
  mode: DisplayCurrencyMode;
  // The space's base currency (Summary.base_currency) — needed to tell
  // "already in base, nothing to convert" apart from "conversion failed,
  // no fx rate" when an account's balance_in_base is null (see
  // resolveDisplayAmount).
  baseCurrency: string;
  // Optional per-row actions menu (omitted for viewers, who can't mutate).
  onRowAction?: (account: AccountWithBalance) => React.ReactNode;
}) {
  const { t } = useTranslation();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("accounts.columns.name")}</TableHead>
          <TableHead>{t("accounts.columns.type")}</TableHead>
          <TableHead>{t("accounts.columns.owner")}</TableHead>
          <TableHead className="text-right">{t("accounts.columns.balance")}</TableHead>
          {onRowAction && <TableHead className="w-10" />}
        </TableRow>
      </TableHeader>
      <TableBody>
        {accounts.map((account) => {
          const archived = account.status === "archived";
          return (
            <TableRow key={account.id} className={cn(archived && "opacity-50")}>
              <TableCell>
                <div className="font-medium">
                  <Link
                    to="/accounts/$accountId"
                    params={{ accountId: account.id }}
                    className="hover:underline"
                  >
                    {account.name}
                  </Link>
                  {archived && (
                    <Badge variant="outline" className="ml-2">
                      {t("accounts.archived")}
                    </Badge>
                  )}
                </div>
                {account.institution && (
                  <div className="text-xs text-muted-foreground">
                    {account.institution}
                  </div>
                )}
              </TableCell>
              <TableCell>
                <Badge variant="secondary">
                  {t(`accountTypes.${account.type}`)}
                </Badge>
              </TableCell>
              <TableCell>
                {account.owner_user_id ? (
                  <Badge variant="outline">{t("accounts.personal")}</Badge>
                ) : (
                  <span className="text-xs text-muted-foreground">
                    {t("accounts.shared")}
                  </span>
                )}
              </TableCell>
              <TableCell className="text-right">
                {account.balance ? (
                  <>
                    <MoneyCell
                      resolved={resolveDisplayAmount(
                        mode,
                        account.currency,
                        account.balance.amount_minor,
                        baseCurrency,
                        account.balance_in_base?.amount_minor,
                      )}
                      className="font-medium tabular-nums"
                      testId={`account-balance-${account.id}`}
                    />
                    <div className="text-xs text-muted-foreground">
                      {formatDate(account.balance.as_of)}
                    </div>
                  </>
                ) : (
                  <span className="text-muted-foreground">—</span>
                )}
              </TableCell>
              {onRowAction && <TableCell>{onRowAction(account)}</TableCell>}
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
