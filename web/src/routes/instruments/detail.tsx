import { useState } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { useSession } from "@/api/session";
import { useAccounts } from "@/api/accounts";
import { ApiError } from "@/api/operations";
import { useInstrumentHoldings, useInstrumentPrices, type InstrumentHolding } from "@/api/instrument-page";
import type { Position } from "@/api/positions";
import { useScreenCurrencies } from "@/lib/screen-currencies";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { CostBasisNotice } from "@/components/cost-basis-notice";
import { QueryGate, RefreshFailedNotice } from "@/components/query-notice";
import { queryState, refreshFailed } from "@/lib/query-state";
import { PositionsTable } from "@/routes/accounts/positions-table";
import { StatePriceDialog, type QuotedPaper } from "@/routes/accounts/state-price-dialog";
import { InstrumentEditDialog } from "@/routes/settings/instruments/edit-dialog";
import type { Instrument } from "@/api/instruments";
import { PriceChart } from "./price-chart";
import { PaperOperations } from "./paper-operations";
import { PaperPayouts } from "./paper-payouts";
import { PaperReturn } from "@/routes/accounts/account-return";
import { priceText } from "./price-text";

type Range = "year" | "all";

// The first day of a stretch back from today: a year, or the ten years the
// server reaches.
function rangeStart(range: Range): string {
  const d = new Date();
  const years = range === "year" ? 1 : 10;
  return new Date(Date.UTC(d.getUTCFullYear() - years, d.getUTCMonth(), d.getUTCDate() + 1))
    .toISOString()
    .slice(0, 10);
}

// Whether a price is old enough that the paper may have stopped being quoted —
// the same month the positions screen calls a price stale after.
function olderThanAMonth(on: string): boolean {
  const monthAgo = new Date();
  monthAgo.setUTCDate(monthAgo.getUTCDate() - 30);
  return on < monthAgo.toISOString().slice(0, 10);
}

// Where a price came from, as the reader would name it.
function sourceLabel(t: TFunction, source: string): string {
  switch (source) {
    case "moex":
    case "moex_history":
      return t("instrumentPage.source.moex");
    case "tinvest":
    case "tinvest_history":
      return t("instrumentPage.source.tinvest");
    case "manual":
      return t("instrumentPage.source.manual");
    default:
      return t("instrumentPage.source.other");
  }
}

// One paper across the family: what it is, how its price went, and what each
// account holds of it — each account's row exactly as that account's own
// positions screen shows it.
export function InstrumentPage() {
  const { t } = useTranslation();
  const { instrumentId } = useParams({ from: "/app/instruments/$instrumentId" });
  const { data: session } = useSession();
  const accounts = useAccounts();
  const holdings = useInstrumentHoldings(instrumentId);
  const [range, setRange] = useState<Range>("year");
  const prices = useInstrumentPrices(instrumentId, rangeStart(range));
  // A year with no prices is not a paper with none: one the exchange stopped
  // quoting has its last price further back, and «цен нет» would be false of
  // it. So an empty year asks for the whole span — the same request «10 лет»
  // makes, which is then answered from the cache.
  const yearEmpty = range === "year" && prices.data?.length === 0;
  const longer = useInstrumentPrices(instrumentId, rangeStart("all"), yearEmpty);
  const [quoting, setQuoting] = useState<QuotedPaper | null>(null);
  const [editing, setEditing] = useState<Instrument | undefined>(undefined);
  const isViewer = session?.role === "viewer";
  const baseCurrency = session?.base_currency ?? "";
  const mode = useScreenCurrencies([
    ...(holdings.data?.holdings ?? []).map((h) => h.position.currency),
    ...(baseCurrency ? [baseCurrency] : []),
  ]);

  if (holdings.error instanceof ApiError && holdings.error.status === 404) {
    return (
      <div className="grid gap-4">
        <Alert variant="destructive">
          <AlertDescription>{t("instrumentPage.notFound")}</AlertDescription>
        </Alert>
        <Link to="/accounts" className="text-sm text-muted-foreground hover:underline">
          {t("accounts.back")}
        </Link>
      </div>
    );
  }
  const state = queryState(holdings, accounts);
  if (state !== "ready" || !holdings.data) return <QueryGate state={state} />;

  const paper = holdings.data.instrument;
  const bond = paper.type === "bond";
  const accountOf = new Map((accounts.data ?? []).map((a) => [a.id, a]));
  const holdingOf = new Map<Position, InstrumentHolding>(holdings.data.holdings.map((h) => [h.position, h]));
  const total = holdings.data.total;
  const series = prices.data ?? [];
  const known = series.length > 0 ? series : (longer.data ?? []);
  const latest = known.length > 0 ? known[known.length - 1] : null;
  const pricesPending = prices.isPending || (yearEmpty && longer.isPending);
  const subtitle = [paper.ticker, paper.isin, t(`instrumentTypes.${paper.type}`), paper.currency]
    .filter(Boolean)
    .join(" · ");

  return (
    <div className="grid gap-6">
      <Link to="/accounts" className="text-sm text-muted-foreground hover:underline">
        {t("accounts.back")}
      </Link>

      <div className="grid gap-1">
        <h1 className="text-2xl font-bold">
          {paper.name}
          {paper.frozen && (
            <Badge variant="outline" className="ml-2 align-middle">
              {t("positions.frozen")}
            </Badge>
          )}
        </h1>
        <div className="text-sm text-muted-foreground">{subtitle}</div>
      </div>

      <Card size="sm">
        <CardContent className="grid gap-2">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            {latest ? (
              <div data-testid="instrument-latest-price">
                <span className="text-xl font-semibold tabular-nums">{priceText(latest, bond)}</span>{" "}
                <span className="text-sm text-muted-foreground">
                  {t("instrumentPage.priceOn", { date: formatDate(latest.on), source: sourceLabel(t, latest.source) })}
                </span>
              </div>
            ) : (
              <div className="grid gap-1 text-sm text-muted-foreground" data-testid="instrument-no-prices">
                {pricesPending ? (
                  t("app.loading")
                ) : paper.ticker !== "" ? (
                  <>
                    {/* A ticker that no source answers for is most often one
                        typed wrong — and nothing else says so: the quote job
                        asks for it every half hour and hears nothing (#35).
                        The correction is one field away, so it is offered
                        here rather than left to be found in the settings. */}
                    <span>{t("instrumentPage.noPricesTicker", { ticker: paper.ticker })}</span>
                    {!isViewer && (
                      <button
                        type="button"
                        className="justify-self-start text-xs underline underline-offset-2"
                        onClick={() => setEditing(paper)}
                      >
                        {t("instrumentPage.fixTicker")}
                      </button>
                    )}
                  </>
                ) : (
                  t("instrumentPage.noPrices")
                )}
              </div>
            )}
            <div className="flex gap-1">
              {(["year", "all"] as const).map((r) => (
                <Button
                  key={r}
                  size="sm"
                  variant={range === r ? "secondary" : "ghost"}
                  onClick={() => setRange(r)}
                >
                  {t(`instrumentPage.range.${r}`)}
                </Button>
              ))}
            </div>
          </div>
          <PriceChart points={series} bond={bond} />
          {bond && <div className="text-xs text-muted-foreground">{t("instrumentPage.bondPercent")}</div>}
          {!isViewer && (!latest || latest.source === "manual" || olderThanAMonth(latest.on)) && (
            <button
              type="button"
              className="justify-self-start text-xs text-muted-foreground underline underline-offset-2"
              onClick={() =>
                setQuoting({ id: paper.id, name: paper.name, currency: paper.currency, bond })
              }
            >
              {t("instrumentPage.statePrice")}
            </button>
          )}
        </CardContent>
      </Card>

      {holdings.data.holdings.length > 0 && (
        <div className="grid gap-1">
          <h2 className="text-lg font-semibold">{t("instrumentPage.return")}</h2>
          <PaperReturn instrumentId={instrumentId} />
        </div>
      )}

      <div className="grid gap-2">
        <h2 className="text-lg font-semibold">{t("instrumentPage.holdings")}</h2>
        <RefreshFailedNotice show={refreshFailed(holdings, accounts)} />
        {holdings.data.holdings.length === 0 ? (
          <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
            {t("instrumentPage.noHoldings")}
          </div>
        ) : (
          <>
            {session && <CostBasisNotice rules={session.cost_basis_rules} namesCountry />}
            <PositionsTable
              positions={holdings.data.holdings.map((h) => h.position)}
              mode={mode}
              baseCurrency={baseCurrency}
              onStatePrice={isViewer ? undefined : setQuoting}
              rowLabel={(position) => {
                const holding = holdingOf.get(position);
                const accountId = holding?.account_id ?? "";
                const account = accountOf.get(accountId);
                return {
                  key: accountId,
                  label: (
                    <>
                      <Link to="/accounts/$accountId" params={{ accountId }} className="hover:underline">
                        {account?.name ?? t("instrumentPage.unknownAccount")}
                      </Link>
                      {account?.status === "archived" && (
                        <Badge variant="outline" className="ml-2">
                          {t("accounts.archived")}
                        </Badge>
                      )}
                    </>
                  ),
                };
              }}
            />
            {total && (
              <div className="text-sm text-muted-foreground" data-testid="instrument-total">
                {t("instrumentPage.total", { quantity: total.quantity })}
                {total.market_value_minor != null &&
                  ` · ${t("instrumentPage.totalValue", { value: formatMinor(total.market_value_minor, total.currency) })}`}
                {total.total_minor != null &&
                  ` · ${t("instrumentPage.totalResult", { value: formatMinor(total.total_minor, total.currency) })}`}
              </div>
            )}
          </>
        )}
      </div>

      {holdings.data.holdings.length > 0 && (
        <PaperPayouts instrumentId={instrumentId} accountName={(id) => accountOf.get(id)?.name} />
      )}

      {holdings.data.holdings.length > 0 && (
        <div className="grid gap-2">
          <h2 className="text-lg font-semibold">{t("instrumentPage.operations")}</h2>
          <PaperOperations instrumentId={instrumentId} accountName={(id) => accountOf.get(id)?.name} />
        </div>
      )}

      {quoting && (
        <StatePriceDialog open onOpenChange={(open) => !open && setQuoting(null)} paper={quoting} />
      )}
      <InstrumentEditDialog instrument={editing} onOpenChange={(open) => !open && setEditing(undefined)} />
    </div>
  );
}
