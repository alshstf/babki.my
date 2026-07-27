import { Link, useParams } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { useAccounts } from "@/api/accounts";
import { usePositions } from "@/api/positions";
import { formatMinor } from "@/lib/money";
import { PositionsTable } from "./positions-table";

export function AccountDetailPage() {
  const { t } = useTranslation();
  const { accountId } = useParams({ from: "/app/accounts/$accountId" });
  const accounts = useAccounts();
  const account = accounts.data?.find((a) => a.id === accountId);
  const positions = usePositions(accountId, !!account);

  if (accounts.isLoading) {
    return <div className="text-muted-foreground">{t("app.loading")}</div>;
  }
  if (accounts.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{t("app.error")}</AlertDescription>
      </Alert>
    );
  }

  if (!account) {
    return (
      <div className="grid gap-4">
        <Alert variant="destructive">
          <AlertDescription>{t("accounts.notFound")}</AlertDescription>
        </Alert>
        <Link to="/accounts" className="text-sm text-muted-foreground hover:underline">
          {t("accounts.back")}
        </Link>
      </div>
    );
  }

  return (
    <div className="grid gap-6">
      <Link to="/accounts" className="text-sm text-muted-foreground hover:underline">
        {t("accounts.back")}
      </Link>

      <div className="grid gap-1">
        <div className="flex items-center gap-2">
          <h1 className="text-2xl font-bold">{account.name}</h1>
          <Badge variant="secondary">{t(`accountTypes.${account.type}`)}</Badge>
        </div>
        <div className="text-sm text-muted-foreground">
          {account.institution && `${account.institution} · `}
          {account.currency}
        </div>
        {account.balance && (
          <div className="mt-2 grid gap-0.5">
            <div className="text-2xl font-bold tabular-nums">
              {formatMinor(account.balance.amount_minor, account.currency)}
            </div>
            <div className="text-xs text-muted-foreground">{account.balance.as_of}</div>
          </div>
        )}
      </div>

      <div className="grid gap-2">
        <h2 className="text-lg font-semibold">{t("positions.title")}</h2>
        {positions.isLoading ? (
          <div className="text-muted-foreground">{t("app.loading")}</div>
        ) : positions.isError ? (
          <Alert variant="destructive">
            <AlertDescription>{t("app.error")}</AlertDescription>
          </Alert>
        ) : positions.data && positions.data.length > 0 ? (
          <PositionsTable positions={positions.data} />
        ) : (
          <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
            {t("positions.empty")}
          </div>
        )}
      </div>

      <div className="grid gap-2">
        {/* Journal (operations list) is added in Task 3; placeholder for now. */}
        <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
          {t("operations.comingSoon")}
        </div>
      </div>
    </div>
  );
}
