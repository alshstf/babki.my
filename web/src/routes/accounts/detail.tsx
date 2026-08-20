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
import { useScreenCurrencies } from "@/lib/screen-currencies";
import { CostBasisNotice } from "@/components/cost-basis-notice";
import { PositionsTable } from "./positions-table";
import { RealizedTotal } from "./realized-total";
import { AccountTotal } from "./account-total";
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
  // The space's base currency comes from the session, which is already
  // loaded app-wide, rather than from GET /api/v1/summary: that endpoint
  // computes a space-wide total this screen never shows, and gating on it
  // meant one failing request replaced the whole account — balance,
  // positions and journal — with an error page.
  const baseCurrency = session?.base_currency ?? "";
  const account = accounts.data?.find((a) => a.id === accountId);
  const positions = usePositions(accountId, !!account);
  const isViewer = session?.role === "viewer";
  const [action, setAction] = useState<AddAction | undefined>(undefined);
  const closeAction = () => setAction(undefined);

  // Reports the currencies in play on this screen so the header's toggle
  // can hide itself when there's nothing to convert (see
  // lib/screen-currencies.tsx): the account's own currency, every
  // position's currency, and the space's base currency (the toggle's
  // conversion target — see the analogous comment in accounts/index.tsx for
  // why that belongs in the set too). The operations journal reports its own
  // currencies separately — it owns its query, including the "show more"
  // window — and the provider counts the union of both reports, so a mode
  // this screen settles on from its own set alone can still change once the
  // journal has spoken. It is handed down to the journal rather than read
  // there, so the two halves of the screen always print in the same currency.
  // Effective, not stored: it only applies while the header toggle is on
  // screen to switch it back off. Must run unconditionally, before any of the
  // early returns below, per the Rules of Hooks.
  const mode = useScreenCurrencies([
    ...(account ? [account.currency] : []),
    ...(positions.data?.positions ?? []).map((p) => p.currency),
    ...(baseCurrency ? [baseCurrency] : []),
  ]);

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

      <div className="grid gap-1">
        <div className="flex items-center gap-2">
          <h1 className="text-2xl font-bold">{account.name}</h1>
          <Badge variant="secondary">{t(`accountTypes.${account.type}`)}</Badge>
        </div>
        <div className="text-sm text-muted-foreground">
          {account.institution && `${account.institution} · `}
          {account.currency}
        </div>
        {/* THE BIGGEST NUMBER ON THE SCREEN ANSWERS THE BIGGEST QUESTION —
            «сколько я тут заработал». It used to be the free cash, which is a
            fact about the account rather than an answer, and which now sits
            with the holdings below. */}
        {positions.data && (
          <AccountTotal total={positions.data.account_total} mode={mode} />
        )}
        {/* One half of the figure above, on its own: what the closed deals
            locked in, which is final and will never move again. It arrives
            added up from the server with the positions it stands over (see
            RealizedTotal in the API contract), and renders nothing at all for
            an account that has neither deals nor a withholding. */}
        {positions.data && (
          <RealizedTotal total={positions.data.realized_total} mode={mode} />
        )}
      </div>

      <div className="grid gap-2">
        <div className="flex items-center justify-between">
          <div className="flex flex-wrap items-baseline gap-x-3">
            <h2 className="text-lg font-semibold">{t("positions.title")}</h2>
          </div>
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
        ) : positions.data &&
          // Money counts as something to show. An account holding nothing but
          // cash used to say «пусто» over a real balance — which was true of
          // its papers and false of the account.
          (positions.data.positions.length > 0 ||
            positions.data.cash.some((c) => c.amount_minor !== 0)) ? (
          <>
            {/* Whether the cost and profit in the table below are the ones
                the owner's country's rules produce. It sits ABOVE the table
                rather than in the session-wide header because it qualifies
                these figures specifically, and it is rendered only when
                there are figures to qualify: over an empty table it would be
                a caveat about nothing. The response carries the statement
                even for an empty account (see the API contract) so that a
                client which needs it earlier still has it. */}
            <CostBasisNotice
              rules={positions.data.cost_basis_rules}
              namesCountry
            />
            <PositionsTable
              positions={positions.data.positions}
              cash={positions.data.cash}
              mode={mode}
              baseCurrency={baseCurrency}
            />
          </>
        ) : (
          <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
            {t("positions.empty")}
          </div>
        )}
      </div>

      <div className="grid gap-2">
        <h2 className="text-lg font-semibold">{t("operations.title")}</h2>
        {/* The cost basis statement comes from the session, which this screen
            has already loaded, and not from the journal response — which since
            #86 does have an envelope to carry one, and deliberately does not:
            a third copy of one truth is a third place to forget it (see
            SessionInfo.cost_basis_rules in the API contract).
            Deliberately NOT positions.data.cost_basis_rules, which is the same
            statement about the same space but reaches this screen only if the
            positions request succeeded: the journal renders on its own and
            must qualify its own figures on its own.

            The table hangs it on the individual amounts that are a cost basis
            rather than over the whole journal — the caveat has to sit on the
            figure it describes, and above a table of fifty rows it would
            describe forty-nine it is not true of. That is also why the notice
            over the positions above is not repeated here word for word. */}
        <OperationsTable
          accountId={accountId}
          canDelete={!isViewer}
          mode={mode}
          baseCurrency={baseCurrency}
          costBasisRules={session?.cost_basis_rules}
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
      {action === "cash" && (
        <CashDialog
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
    </div>
  );
}
