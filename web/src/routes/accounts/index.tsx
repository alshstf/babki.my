import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { useSession } from "@/api/session";
import {
  useAccounts,
  useArchiveAccount,
  useSummary,
  useUpdateAccount,
  type AccountWithBalance,
} from "@/api/accounts";
import { useScreenCurrencies } from "@/lib/screen-currencies";
import { SummaryCards } from "./summary-cards";
import { CapitalChart } from "./capital-chart";
import { FamilyReturnLine } from "./account-return";
import { AccountsTable } from "./accounts-table";
import { AccountDialog } from "./account-dialog";
import { BalanceDialog } from "./balance-dialog";
import { RowMenu } from "./row-menu";
import { QueryGate, RefreshFailedNotice } from "@/components/query-notice";
import { StaleSourcesNotice } from "@/components/data-sources";
import { queryState, refreshFailed } from "@/lib/query-state";

export function AccountsPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const accounts = useAccounts();
  const summary = useSummary();
  const archiveAccount = useArchiveAccount();
  const valueBy = useUpdateAccount();
  const restore = useUpdateAccount();

  // undefined = dialog closed, null = create mode, account = edit mode.
  const [dialogAccount, setDialogAccount] = useState<AccountWithBalance | null | undefined>(
    undefined,
  );
  const [archiveTarget, setArchiveTarget] = useState<AccountWithBalance | null>(null);
  const [balanceTarget, setBalanceTarget] = useState<AccountWithBalance | null>(null);

  const isViewer = session?.role === "viewer";

  // The screen's currencies plus the base currency (so one shared foreign
  // currency still counts as two) for the header toggle; returns the effective
  // mode. Before the early returns (Rules of Hooks); reports nothing until data
  // arrives.
  const mode = useScreenCurrencies([
    ...(accounts.data ?? []).map((a) => a.currency),
    ...(summary.data ? [summary.data.base_currency] : []),
  ]);

  // Data that is already here stays on screen when a refresh of it fails, and
  // a request the browser has not sent is not an empty list (see query-state).
  const state = queryState(accounts, summary);
  if (state !== "ready") return <QueryGate state={state} />;

  const list = accounts.data ?? [];
  // Defensive: the gates above guarantee summary.data, which TS cannot
  // narrow.
  const baseCurrency = summary.data?.base_currency ?? "";

  const confirmArchive = () => {
    if (!archiveTarget) return;
    archiveAccount.mutate(archiveTarget.id, {
      onSuccess: () => setArchiveTarget(null),
    });
  };

  return (
    <div className="grid gap-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">{t("accounts.title")}</h1>
        {!isViewer && (
          <Button onClick={() => setDialogAccount(null)}>{t("accounts.add")}</Button>
        )}
      </div>
      <RefreshFailedNotice show={refreshFailed(accounts, summary)} />
      <StaleSourcesNotice canOpenSettings={session?.role === "owner"} />
      {summary.data && <SummaryCards summary={summary.data} mode={mode} />}
      <CapitalChart />
      <FamilyReturnLine />
      {valueBy.isError && (
        <Alert variant="destructive">
          <AlertDescription>{t("accounts.journal.switchError")}</AlertDescription>
        </Alert>
      )}
      {restore.isError && (
        <Alert variant="destructive">
          <AlertDescription>{t("accounts.restoreError")}</AlertDescription>
        </Alert>
      )}
      {list.length === 0 ? (
        <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
          {t("accounts.empty")}
        </div>
      ) : (
        <AccountsTable
          accounts={list}
          mode={mode}
          baseCurrency={baseCurrency}
          onValueBy={
            isViewer
              ? undefined
              : (account, byBalance) =>
                  valueBy.mutate({ id: account.id, body: { valued_by_balance: byBalance } })
          }
          switching={valueBy.isPending ? valueBy.variables?.id : undefined}
          onRowAction={
            isViewer
              ? undefined
              : (account) => (
                  <RowMenu
                    account={account}
                    onEdit={setDialogAccount}
                    onBalance={setBalanceTarget}
                    onArchive={setArchiveTarget}
                    onRestore={(target) =>
                      restore.mutate({ id: target.id, body: { status: "active" } })
                    }
                  />
                )
          }
        />
      )}

      <AccountDialog
        open={dialogAccount !== undefined}
        onOpenChange={(open) => !open && setDialogAccount(undefined)}
        account={dialogAccount ?? undefined}
      />

      <BalanceDialog
        open={balanceTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setBalanceTarget(null);
          }
        }}
        account={balanceTarget ?? undefined}
      />

      <Dialog
        open={archiveTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setArchiveTarget(null);
            archiveAccount.reset();
          }
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t("accounts.menu.archive")}</DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground">
            {t("accounts.archiveConfirm", { name: archiveTarget?.name ?? "" })}
          </p>
          {/* Names the action that did not happen; the server's English is
             not part of the contract. */}
          {archiveAccount.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("accounts.archiveError")}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => {
                // Reset here too: Radix calls onOpenChange only for its own dismiss
                // triggers, so a refused archive's alert would greet the next account
                // (#21), as in operations-table.tsx.
                setArchiveTarget(null);
                archiveAccount.reset();
              }}
            >
              {t("common.cancel")}
            </Button>
            <Button
              variant="destructive"
              disabled={archiveAccount.isPending}
              onClick={confirmArchive}
            >
              {t("accounts.menu.archive")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
