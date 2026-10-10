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
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useSession } from "@/api/session";
import { useAccounts } from "@/api/accounts";
import { usePositions } from "@/api/positions";
import { useScreenCurrencies } from "@/lib/screen-currencies";
import { CostBasisNotice } from "@/components/cost-basis-notice";
import { PositionsTable } from "./positions-table";
import { RealizedTotal } from "./realized-total";
import { AccountTotal } from "./account-total";
import { AccountReturn } from "./account-return";
import { OperationsTable } from "./operations-table";
import { TradeDialog } from "./trade-dialog";
import { CashDialog } from "./cash-dialog";
import { MoneyTransferDialog } from "./money-transfer-dialog";
import { IncomeDialog } from "./income-dialog";
import { TransferDialog } from "./transfer-dialog";
import { ArrivalDialog } from "./arrival-dialog";
import { PurchasePriceDialog, type PricedPaper } from "./purchase-price-dialog";
import { StatePriceDialog, type QuotedPaper } from "./state-price-dialog";
import { OpeningBalanceDialog } from "./opening-balance-dialog";
import type { CashPosition } from "@/api/positions";
import { editDialogOf, type Operation } from "@/api/operations";
import type { Instrument } from "@/api/instruments";
import { QueryGate, RefreshFailedNotice } from "@/components/query-notice";
import { AccountBusyNotice } from "@/components/background-activity";
import { queryState, refreshFailed } from "@/lib/query-state";

// undefined = no dialog open; otherwise the action picked from the
// "+ Add operation" menu, each mapping to one dialog below.
type AddAction = "buy" | "sell" | "cash" | "money" | "income" | "transfer" | "arrival" | "spent" | "earned";

export function AccountDetailPage() {
  const { t } = useTranslation();
  const { accountId } = useParams({ from: "/app/accounts/$accountId" });
  const { data: session } = useSession();
  const accounts = useAccounts();
  // The base currency from the session, not GET /summary, whose failure
  // would replace the whole account with an error.
  const baseCurrency = session?.base_currency ?? "";
  const account = accounts.data?.find((a) => a.id === accountId);
  const positions = usePositions(accountId, !!account);
  const isViewer = session?.role === "viewer";
  // Archived accounts are read-only until restored (the server refuses hand
  // entries); viewers always.
  const readOnly = isViewer || account?.status === "archived";
  const [action, setAction] = useState<AddAction | undefined>(undefined);
  const closeAction = () => setAction(undefined);
  // The paper whose purchase price is being given, from its «указать цену».
  const [pricing, setPricing] = useState<PricedPaper | null>(null);
  const [quoting, setQuoting] = useState<QuotedPaper | null>(null);
  // The currency gone below zero whose opening balance is being given.
  const [opening, setOpening] = useState<(CashPosition & { overdrawn_since: string }) | null>(null);
  // The recorded operation being corrected, and the paper it names.
  const [editing, setEditing] = useState<{
    operation: Operation;
    instrument: Instrument | null;
  } | null>(null);

  // The screen's currencies (the account's, every position's and the base
  // currency) for the header toggle (see lib/screen-currencies.tsx). The journal
  // reports its own; the provider counts the union. The mode is handed to the
  // journal so both halves print in one currency. Effective, not stored. Before the
  // early returns (Rules of Hooks).
  const mode = useScreenCurrencies([
    ...(account ? [account.currency] : []),
    ...(positions.data?.positions ?? []).map((p) => p.currency),
    ...(baseCurrency ? [baseCurrency] : []),
  ]);

  const accountsState = queryState(accounts);
  if (accountsState !== "ready") return <QueryGate state={accountsState} />;
  const positionsState = queryState(positions);

  if (!account) {
    return (
      <div className="grid gap-4">
        <Alert variant="destructive">
          <AlertDescription>{t("accounts.notFound")}</AlertDescription>
        </Alert>
        <Link
          to="/accounts"
          className="text-sm text-muted-foreground hover:underline"
        >
          {t("accounts.back")}
        </Link>
      </div>
    );
  }

  return (
    <div className="grid gap-6">
      <Link
        to="/accounts"
        className="text-sm text-muted-foreground hover:underline"
      >
        {t("accounts.back")}
      </Link>

      {/* Above the figures, which are what is not final while a job runs. */}
      <AccountBusyNotice accountId={accountId} />

      <div className="grid gap-1">
        <div className="flex items-center gap-2">
          <h1 className="text-2xl font-bold">{account.name}</h1>
          <Badge variant="secondary">{t(`accountTypes.${account.type}`)}</Badge>
        </div>
        <div className="text-sm text-muted-foreground">
          {account.institution && `${account.institution} · `}
          {account.currency}
        </div>
        {/* The biggest number answers «сколько я тут заработал»; the free cash
           sits with the holdings below. */}
        {positions.data && (
          <AccountTotal total={positions.data.account_total} mode={mode} />
        )}
        {/* The closed deals' part of the figure above, final; nothing at all
           for an account with no deals and no withholding. */}
        {positions.data && (
          <RealizedTotal total={positions.data.realized_total} mode={mode} />
        )}
        {account.type === "brokerage" && <AccountReturn accountId={accountId} />}
      </div>

      {account.status === "archived" && (
        <Alert>
          <AlertDescription>{t("accounts.archivedNotice")}</AlertDescription>
        </Alert>
      )}

      <div className="grid gap-2">
        <div className="flex items-center justify-between">
          <div className="flex flex-wrap items-baseline gap-x-3">
            <h2 className="text-lg font-semibold">{t("positions.title")}</h2>
          </div>
          {!readOnly && (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="outline" size="sm">
                  {t("accounts.addOperation")}
                  <ChevronDown className="size-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent>
                {/* A card, a deposit or cash is where a family spends and earns:
                   those two come first there. */}
                {account.type !== "brokerage" && (
                  <>
                    <DropdownMenuItem onSelect={() => setAction("spent")}>
                      {t("cash.presetMenu.expense")}
                    </DropdownMenuItem>
                    <DropdownMenuItem onSelect={() => setAction("earned")}>
                      {t("cash.presetMenu.income")}
                    </DropdownMenuItem>
                    <DropdownMenuSeparator />
                  </>
                )}
                <DropdownMenuItem onSelect={() => setAction("buy")}>
                  {t("trade.buyTitle")}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setAction("sell")}>
                  {t("trade.sellTitle")}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setAction("cash")}>
                  {t("cash.menuItem")}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setAction("money")}>
                  {t("moneyTransfer.menuItem")}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setAction("income")}>
                  {t("income.menuItem")}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setAction("transfer")}>
                  {t("transfer.title")}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setAction("arrival")}>
                  {t("arrival.menuItem")}
                </DropdownMenuItem>
                <DropdownMenuItem asChild>
                  <Link to="/accounts/$accountId/import" params={{ accountId }}>
                    {t("tableImport.menuItem")}
                  </Link>
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </div>
        <RefreshFailedNotice show={refreshFailed(accounts, positions)} />
        {positionsState !== "ready" ? (
          <QueryGate state={positionsState} />
        ) : positions.data &&
          // Money counts as something to show, or a cash-only account read
          // «пусто».
          (positions.data.positions.length > 0 ||
            positions.data.cash.some((c) => c.amount_minor !== 0)) ? (
          <>
            {/* Whether cost and profit below follow the owner's country's rules,
               over the table it qualifies and only when it has figures. */}
            <CostBasisNotice
              rules={positions.data.cost_basis_rules}
              namesCountry
            />
            <PositionsTable
              positions={positions.data.positions}
              cash={positions.data.cash}
              mode={mode}
              baseCurrency={baseCurrency}
              onPriceUnknown={readOnly ? undefined : setPricing}
              onStatePrice={isViewer ? undefined : setQuoting}
              onOpeningBalance={readOnly ? undefined : setOpening}
              instrumentLinks
            />
          </>
        ) : (
          <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
            {t("positions.empty")}
          </div>
        )}
      </div>

      <div className="grid gap-2">
        <div className="flex items-center justify-between">
          <h2 className="text-lg font-semibold">{t("operations.title")}</h2>
          {/* A plain link: the session cookie goes with it, and the browser
              saves what the server names. */}
          <a
            href={`/api/v1/accounts/${accountId}/journal.csv`}
            download
            className="text-sm text-muted-foreground underline underline-offset-2 hover:text-foreground"
            title={t("operations.exportHint")}
          >
            {t("operations.export")}
          </a>
        </div>
        {/* The cost-basis statement comes from the session, not the journal or
           positions response, so the journal qualifies its own figures even if
           positions failed. The table hangs it on the amounts that are a cost
           basis, not over the whole journal. */}
        <OperationsTable
          accountId={accountId}
          canDelete={!readOnly}
          mode={mode}
          baseCurrency={baseCurrency}
          costBasisRules={session?.cost_basis_rules}
          onPurchasePrice={readOnly ? undefined : setPricing}
          onEdit={readOnly ? undefined : (operation, instrument) => setEditing({ operation, instrument })}
          accountName={(id) => accounts.data?.find((a) => a.id === id)?.name}
          instrumentLinks
          papers={(positions.data?.positions ?? []).map((p) => ({
            id: p.instrument.id,
            name: p.instrument.name,
          }))}
        />
      </div>

      {(action === "buy" || action === "sell") && (
        <TradeDialog
          open
          onOpenChange={(open) => !open && closeAction()}
          account={account}
          side={action}
        />
      )}
      {(action === "cash" || action === "spent" || action === "earned") && (
        <CashDialog
          open
          onOpenChange={(open) => !open && closeAction()}
          account={account}
          preset={action === "spent" ? "expense" : action === "earned" ? "income" : undefined}
        />
      )}
      {action === "money" && (
        <MoneyTransferDialog
          open
          onOpenChange={(open) => !open && closeAction()}
          account={account}
        />
      )}
      {action === "income" && (
        <IncomeDialog
          open
          onOpenChange={(open) => !open && closeAction()}
          account={account}
        />
      )}
      {action === "transfer" && (
        <TransferDialog
          open
          onOpenChange={(open) => !open && closeAction()}
          account={account}
        />
      )}
      {action === "arrival" && (
        <ArrivalDialog
          open
          onOpenChange={(open) => !open && closeAction()}
          account={account}
        />
      )}
      {editing && editDialogOf(editing.operation) === "trade" && (
        <TradeDialog
          open
          onOpenChange={(open) => !open && setEditing(null)}
          account={account}
          side={editing.operation.type === "sell" ? "sell" : "buy"}
          editing={editing.operation}
          editingInstrument={editing.instrument}
        />
      )}
      {editing && editDialogOf(editing.operation) === "cash" && (
        <CashDialog
          open
          onOpenChange={(open) => !open && setEditing(null)}
          account={account}
          editing={editing.operation}
        />
      )}
      {editing && editDialogOf(editing.operation) === "income" && (
        <IncomeDialog
          open
          onOpenChange={(open) => !open && setEditing(null)}
          account={account}
          editing={editing.operation}
          editingInstrument={editing.instrument}
        />
      )}
      {quoting && (
        <StatePriceDialog open onOpenChange={(open) => !open && setQuoting(null)} paper={quoting} />
      )}
      {opening && (
        <OpeningBalanceDialog
          open
          onOpenChange={(open) => !open && setOpening(null)}
          account={account}
          money={opening}
        />
      )}
      {pricing && (
        <PurchasePriceDialog
          open
          onOpenChange={(open) => !open && setPricing(null)}
          accountId={accountId}
          paper={pricing}
        />
      )}
    </div>
  );
}
