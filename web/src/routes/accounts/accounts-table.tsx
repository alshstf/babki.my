import { useTranslation } from "react-i18next";
import { LoaderCircle } from "lucide-react";
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
import { formatMinor, signClass } from "@/lib/money";
import type { AccountWithBalance } from "@/api/accounts";
import { JournalNotes } from "./journal-notes";
import { useNarrow } from "@/lib/use-narrow";

export function AccountsTable({
  accounts,
  mode,
  baseCurrency,
  onRowAction,
  onValueBy,
  switching,
  busy,
}: {
  accounts: AccountWithBalance[];
  // The accounts a running job is still changing (BackgroundActivity).
  busy?: ReadonlySet<string>;
  mode: DisplayCurrencyMode;
  // The base currency, to tell "nothing to convert" from "no rate" when
  // balance_in_base is null (see resolveDisplayAmount).
  baseCurrency: string;
  // Optional per-row actions menu (omitted for viewers, who can't mutate).
  onRowAction?: (account: AccountWithBalance) => React.ReactNode;
  // Switches an account between its journal and its balance (omitted for
  // viewers), and the account a switch is under way for.
  onValueBy?: (account: AccountWithBalance, byBalance: boolean) => void;
  switching?: string;
}) {
  const { t } = useTranslation();
  const narrow = useNarrow();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("accounts.columns.name")}</TableHead>
          {!narrow && <TableHead>{t("accounts.columns.type")}</TableHead>}
          {!narrow && <TableHead>{t("accounts.columns.owner")}</TableHead>}
          <TableHead className="text-right">{t("accounts.columns.balance")}</TableHead>
          {onRowAction && <TableHead className="w-10" />}
        </TableRow>
      </TableHeader>
      <TableBody>
        {accounts.map((account) => {
          const archived = account.status === "archived";
          return (
            <TableRow key={account.id} className={cn(archived && "opacity-50")}>
              <TableCell className={narrow ? "whitespace-normal" : undefined}>
                <div className="font-medium">
                  <Link
                    to="/accounts/$accountId"
                    params={{ accountId: account.id }}
                    className="hover:underline"
                  >
                    {account.name}
                  </Link>
                  {busy?.has(account.id) && (
                    <LoaderCircle
                      className="ml-2 inline size-3.5 animate-spin align-[-2px] text-muted-foreground"
                      role="img"
                      aria-label={t("background.accountBusy")}
                      data-testid="account-busy"
                    >
                      <title>{t("background.accountBusy")}</title>
                    </LoaderCircle>
                  )}
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
                {/* On a phone the kind and whose it is fold under the name. */}
                {narrow && (
                  <div className="text-xs text-muted-foreground">
                    {t(`accountTypes.${account.type}`)} ·{" "}
                    {account.owner_user_id ? t("accounts.personal") : t("accounts.shared")}
                  </div>
                )}
              </TableCell>
              {!narrow && (
                <TableCell>
                  <Badge variant="secondary">
                    {t(`accountTypes.${account.type}`)}
                  </Badge>
                </TableCell>
              )}
              {!narrow && (
                <TableCell>
                  {account.owner_user_id ? (
                    <Badge variant="outline">{t("accounts.personal")}</Badge>
                  ) : (
                    <span className="text-xs text-muted-foreground">
                      {t("accounts.shared")}
                    </span>
                  )}
                </TableCell>
              )}
              <TableCell className="text-right">
                {account.journal && account.counted_by === "journal" ? (
                  // Counted by its journal: the figure is already in the base
                  // currency, whatever the account's own, because a journal
                  // holds several currencies and the server sums them there.
                  <div
                    data-testid={`account-journal-value-${account.id}`}
                    className={cn("font-medium tabular-nums", signClass(account.journal.amount_minor))}
                  >
                    {formatMinor(account.journal.amount_minor, account.journal.currency)}
                  </div>
                ) : account.balance ? (
                  <>
                    <MoneyCell
                      resolved={resolveDisplayAmount(
                        mode,
                        account.currency,
                        account.balance.amount_minor,
                        baseCurrency,
                        // In the currency the converted balance carries
                        // (MoneyInBase.currency), not the session's, which a cached row
                        // may have outlived (#106).
                        account.balance_in_base && {
                          amountMinor: account.balance_in_base.amount_minor,
                          currency: account.balance_in_base.currency,
                          rateOn: account.balance_in_base.rate_on,
                        },
                      )}
                      className="font-medium tabular-nums"
                      testId={`account-balance-${account.id}`}
                    />
                    <div className="text-xs text-muted-foreground">
                      {formatDate(account.balance.as_of)}
                    </div>
                  </>
                ) : (
                  // No balance mark was ever recorded: the server publishes none
                  // (#31). Says what is absent, not who failed to enter it, since
                  // imports write marks too.
                  <span
                    data-testid={`account-balance-${account.id}-none`}
                    className="text-muted-foreground"
                    title={t("accounts.noBalanceRecorded")}
                  >
                    <span aria-hidden="true">—</span>
                    <span className="sr-only">{t("accounts.noBalanceRecorded")}</span>
                  </span>
                )}
                {account.journal && (
                  <JournalNotes
                    account={account}
                    journal={account.journal}
                    onValueBy={onValueBy}
                    pending={switching === account.id}
                  />
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
