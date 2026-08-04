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
import { useAccounts, useArchiveAccount, useSummary, type AccountWithBalance } from "@/api/accounts";
import {
  useEffectiveDisplayCurrencyMode,
  useReportScreenCurrencies,
} from "@/lib/screen-currencies";
import { SummaryCards } from "./summary-cards";
import { AccountsTable } from "./accounts-table";
import { AccountDialog } from "./account-dialog";
import { BalanceDialog } from "./balance-dialog";
import { RowMenu } from "./row-menu";

export function AccountsPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const accounts = useAccounts();
  const summary = useSummary();
  const archiveAccount = useArchiveAccount();
  // Effective, not stored: the mode only applies while the header toggle is
  // on screen to switch it back off (see useEffectiveDisplayCurrencyMode).
  const mode = useEffectiveDisplayCurrencyMode();

  // undefined = dialog closed, null = create mode, account = edit mode.
  const [dialogAccount, setDialogAccount] = useState<AccountWithBalance | null | undefined>(
    undefined,
  );
  const [archiveTarget, setArchiveTarget] = useState<AccountWithBalance | null>(null);
  const [balanceTarget, setBalanceTarget] = useState<AccountWithBalance | null>(null);

  const isViewer = session?.role === "viewer";

  // Reports the currencies in play on this screen so the header's toggle
  // can hide itself when there's nothing to convert (see
  // lib/screen-currencies.tsx). Includes the base currency alongside the
  // accounts' own currencies: even a screen where every account happens to
  // share one *foreign* currency still has something meaningful for the
  // toggle to convert into, so that case must count as 2, not 1. Must run
  // unconditionally (before the loading/error returns below) per the Rules
  // of Hooks — accounts.data/summary.data are simply undefined pre-load, so
  // this naturally reports 0 currencies (toggle hidden) until data arrives.
  useReportScreenCurrencies([
    ...(accounts.data ?? []).map((a) => a.currency),
    ...(summary.data ? [summary.data.base_currency] : []),
  ]);

  if (accounts.isLoading || summary.isLoading) {
    return <div className="text-muted-foreground">{t("app.loading")}</div>;
  }
  if (accounts.isError || summary.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{t("app.error")}</AlertDescription>
      </Alert>
    );
  }

  const list = accounts.data ?? [];
  // Defensive fallback only — by this point accounts.isLoading/isError and
  // summary.isLoading/isError have already gated the render above, so
  // summary.data is expected to be defined; TS just can't narrow that from
  // those boolean checks alone.
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
      {summary.data && <SummaryCards summary={summary.data} mode={mode} />}
      {list.length === 0 ? (
        <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
          {t("accounts.empty")}
        </div>
      ) : (
        <AccountsTable
          accounts={list}
          mode={mode}
          baseCurrency={baseCurrency}
          onRowAction={
            isViewer
              ? undefined
              : (account) => (
                  <RowMenu
                    account={account}
                    onEdit={setDialogAccount}
                    onBalance={setBalanceTarget}
                    onArchive={setArchiveTarget}
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
          {/* Names the action that did not happen, and nothing else: the
              server's own message is English prose written for a log, and it is
              not part of the contract this client is written against (only the
              status is — see api/openapi.yaml). */}
          {archiveAccount.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("accounts.archiveError")}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setArchiveTarget(null)}>
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
