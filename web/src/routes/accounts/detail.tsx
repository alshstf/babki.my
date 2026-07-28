import { useState } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { ChevronDown } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useSession } from "@/api/session";
import { useAccounts } from "@/api/accounts";
import { usePositions } from "@/api/positions";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { PositionsTable } from "./positions-table";
import { OperationsTable } from "./operations-table";
import { TradeDialog } from "./trade-dialog";
import { CashDialog } from "./cash-dialog";
import { IncomeDialog } from "./income-dialog";
import { TransferDialog } from "./transfer-dialog";

// undefined = no dialog open; otherwise the action picked from the
// "+ Add operation" menu, each mapping to one dialog below.
type AddAction = "buy" | "sell" | "cash" | "income" | "transfer";

export function AccountDetailPage() {
  const { t } = useTranslation();
  const { accountId } = useParams({ from: "/app/accounts/$accountId" });
  const { data: session } = useSession();
  const accounts = useAccounts();
  const account = accounts.data?.find((a) => a.id === accountId);
  const positions = usePositions(accountId, !!account);
  const isViewer = session?.role === "viewer";
  const [action, setAction] = useState<AddAction | undefined>(undefined);
  const closeAction = () => setAction(undefined);

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
            <div className="text-xs text-muted-foreground">{formatDate(account.balance.as_of)}</div>
          </div>
        )}
      </div>

      <div className="grid gap-2">
        <div className="flex items-center justify-between">
          <h2 className="text-lg font-semibold">{t("positions.title")}</h2>
          {!isViewer && (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="outline" size="sm">
                  {t("accounts.addOperation")}
                  <ChevronDown className="size-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent>
                <DropdownMenuItem onSelect={() => setAction("buy")}>
                  {t("trade.buyTitle")}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setAction("sell")}>
                  {t("trade.sellTitle")}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setAction("cash")}>
                  {t("cash.menuItem")}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setAction("income")}>
                  {t("income.menuItem")}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setAction("transfer")}>
                  {t("transfer.title")}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </div>
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
        <h2 className="text-lg font-semibold">{t("operations.title")}</h2>
        <OperationsTable accountId={accountId} canDelete={!isViewer} />
      </div>

      {(action === "buy" || action === "sell") && (
        <TradeDialog
          open
          onOpenChange={(open) => !open && closeAction()}
          account={account}
          side={action}
        />
      )}
      {action === "cash" && (
        <CashDialog open onOpenChange={(open) => !open && closeAction()} account={account} />
      )}
      {action === "income" && (
        <IncomeDialog open onOpenChange={(open) => !open && closeAction()} account={account} />
      )}
      {action === "transfer" && (
        <TransferDialog open onOpenChange={(open) => !open && closeAction()} account={account} />
      )}
    </div>
  );
}
