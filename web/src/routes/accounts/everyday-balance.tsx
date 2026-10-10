import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { cn } from "@/lib/utils";
import type { AccountWithBalance } from "@/api/accounts";
import type { CashPosition } from "@/api/positions";
import { isDebt } from "@/lib/account-kinds";

// EverydayBalance is what a card, a current account, a deposit or cash holds:
// by its operations, by the bank's last balance, how the two agree, and which
// of them the family total counts. A broker's holdings table and «заработано»
// mean nothing here (alshstf/babki.my#413).
export function EverydayBalance({
  account,
  cash,
  onKeep,
  pending,
  onOpeningBalance,
}: {
  account: AccountWithBalance;
  // The money the journal leaves, by currency; empty when there is no journal.
  cash: CashPosition[];
  // Switches the account between its operations and its balance; absent for
  // someone who cannot change it.
  onKeep?: (byOperations: boolean) => void;
  pending?: boolean;
  onOpeningBalance?: (money: CashPosition & { overdrawn_since: string }) => void;
}) {
  const { t } = useTranslation();
  const debt = isDebt(account);
  const byOperations = account.counted_by === "journal";
  const rec = account.journal?.reconciliation;
  const held = cash.filter((c) => c.amount_minor !== 0);

  return (
    <div className="grid gap-3 rounded-lg border p-4" data-testid="everyday-balance">
      <div className="grid gap-3 sm:grid-cols-2">
        <div>
          <div className="text-sm text-muted-foreground">
            {debt ? t("everyday.debtByOperations") : t("everyday.byOperations")}
          </div>
          {held.length === 0 ? (
            <div className="text-2xl font-semibold tabular-nums">—</div>
          ) : (
            held.map((c) => (
              <div
                key={c.currency}
                className={cn(
                  "text-2xl font-semibold tabular-nums",
                  !debt && c.amount_minor < 0 && "text-red-600 dark:text-red-400",
                )}
                data-testid="everyday-by-operations"
              >
                {/* A debt reads as what is owed, not as minus money. */}
                {formatMinor(debt ? Math.abs(c.amount_minor) : c.amount_minor, c.currency)}
              </div>
            ))
          )}
        </div>
        <div>
          <div className="text-sm text-muted-foreground">{t("everyday.byBank")}</div>
          {account.balance ? (
            <>
              <div className="text-2xl font-semibold tabular-nums" data-testid="everyday-by-bank">
                {formatMinor(debt ? Math.abs(account.balance.amount_minor) : account.balance.amount_minor, account.currency)}
              </div>
              <div className="text-xs text-muted-foreground">
                {t("everyday.asOf", { date: formatDate(account.balance.as_of) })}
              </div>
            </>
          ) : (
            <div className="text-sm text-muted-foreground">{t("everyday.noBalance")}</div>
          )}
        </div>
      </div>
      {rec && (
        <div
          className={cn(
            "text-sm",
            rec.status === "agrees" && "text-emerald-600",
            rec.status === "differs" && "text-red-600",
            (rec.status === "close" || rec.status === "stale") && "text-muted-foreground",
          )}
          data-testid="everyday-reconciliation"
        >
          {t(`everyday.reconciliation.${rec.status}`, {
            difference: formatMinor(Math.abs(rec.difference_minor), account.journal?.currency ?? account.currency),
          })}
        </div>
      )}
      {!debt &&
        cash.map(
          (c) =>
            c.overdrawn_since != null &&
            onOpeningBalance && (
              <div key={c.currency} className="text-sm text-amber-700 dark:text-amber-400">
                {t("everyday.overdrawn", { date: formatDate(c.overdrawn_since) })}{" "}
                <button
                  type="button"
                  className="underline underline-offset-2"
                  onClick={() => onOpeningBalance({ ...c, overdrawn_since: c.overdrawn_since as string })}
                >
                  {t("everyday.openingBalance")}
                </button>
              </div>
            ),
        )}
      <div className="flex flex-wrap items-center gap-3 text-sm text-muted-foreground">
        <span data-testid="everyday-counted-by">
          {byOperations ? t("everyday.countedByOperations") : t("everyday.countedByBank")}
        </span>
        {onKeep && (
          <Button size="sm" variant="outline" disabled={pending} onClick={() => onKeep(!byOperations)}>
            {byOperations ? t("everyday.keepByBank") : t("everyday.keepByOperations")}
          </Button>
        )}
      </div>
    </div>
  );
}
