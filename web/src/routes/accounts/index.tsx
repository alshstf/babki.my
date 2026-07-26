import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useAccounts, useSummary } from "@/api/accounts";
import { SummaryCards } from "./summary-cards";
import { AccountsTable } from "./accounts-table";

export function AccountsPage() {
  const { t } = useTranslation();
  const accounts = useAccounts();
  const summary = useSummary();

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
  return (
    <div className="grid gap-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">{t("accounts.title")}</h1>
        {/* Create button arrives in Task 5 */}
      </div>
      {summary.data && <SummaryCards summary={summary.data} />}
      {list.length === 0 ? (
        <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
          {t("accounts.empty")}
        </div>
      ) : (
        <AccountsTable accounts={list} />
      )}
    </div>
  );
}
