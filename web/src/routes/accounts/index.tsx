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
import { SummaryCards } from "./summary-cards";
import { AccountsTable } from "./accounts-table";
import { AccountDialog } from "./account-dialog";
import { RowMenu } from "./row-menu";

export function AccountsPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const accounts = useAccounts();
  const summary = useSummary();
  const archiveAccount = useArchiveAccount();

  // undefined = dialog closed, null = create mode, account = edit mode.
  const [dialogAccount, setDialogAccount] = useState<AccountWithBalance | null | undefined>(
    undefined,
  );
  const [archiveTarget, setArchiveTarget] = useState<AccountWithBalance | null>(null);

  const isViewer = session?.role === "viewer";

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
      {summary.data && <SummaryCards summary={summary.data} />}
      {list.length === 0 ? (
        <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
          {t("accounts.empty")}
        </div>
      ) : (
        <AccountsTable
          accounts={list}
          onRowAction={
            isViewer
              ? undefined
              : (account) => (
                  <RowMenu
                    account={account}
                    onEdit={setDialogAccount}
                    onBalance={() => {}}
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
          {archiveAccount.isError && (
            <Alert variant="destructive">
              <AlertDescription>{archiveAccount.error.message}</AlertDescription>
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
