import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { Link } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { useDataSources, type DataSource } from "@/api/data-sources";
import { formatDateTime } from "@/lib/dates";

// What a reader calls a source; the server names it by its job.
function sourceName(t: TFunction, kind: string): string {
  switch (kind) {
    case "marketdata.refresh_quotes":
      return t("dataSources.kinds.moexQuotes");
    case "marketdata.backfill_quotes":
      return t("dataSources.kinds.moexHistory");
    case "marketdata.refresh_fx":
      return t("dataSources.kinds.cbrRates");
    case "marketdata.backfill_fx":
      return t("dataSources.kinds.cbrHistory");
    case "tinvest.sync":
      return t("dataSources.kinds.tinvestSync");
    case "tinvest.refresh_quotes":
      return t("dataSources.kinds.tinvestQuotes");
    case "tinvest.backfill_quotes":
      return t("dataSources.kinds.tinvestHistory");
    case "tinvest.refresh_dividends":
      return t("dataSources.kinds.tinvestDividends");
    case "corporateaction.refresh_moex_splits":
      return t("dataSources.kinds.moexSplits");
    default:
      return kind;
  }
}

// A failure that came after the last success is the source's present state;
// an older one is history.
function failingNow(s: DataSource): boolean {
  if (!s.last_failure_at) return false;
  return !s.last_success_at || s.last_failure_at > s.last_success_at;
}

// Each source of outside data with when it last worked and, when it is failing
// now, how.
export function DataSourcesList() {
  const { t } = useTranslation();
  const sources = useDataSources();
  if (!sources.data) return null;
  return (
    <ul className="grid gap-2 text-sm" data-testid="data-sources">
      {sources.data.map((s) => (
        <li key={s.kind} className="grid gap-0.5">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-medium">{sourceName(t, s.kind)}</span>
            {s.stale && <Badge variant="destructive">{t("dataSources.stale")}</Badge>}
          </div>
          <div className="text-xs text-muted-foreground">
            {s.last_success_at
              ? t("dataSources.lastSuccess", { at: formatDateTime(s.last_success_at) })
              : s.last_failure_at
                ? t("dataSources.neverSucceeded")
                : t("dataSources.notRunYet")}
          </div>
          {failingNow(s) && s.last_failure_at && (
            <div className="text-xs text-red-600 break-words">
              {t("dataSources.lastFailure", { at: formatDateTime(s.last_failure_at), error: s.last_error })}
            </div>
          )}
        </li>
      ))}
    </ul>
  );
}

// The prices and rates every figure is struck from: a warning on the screen of
// figures when one of their sources has stopped, so that the figures are not
// read as today's.
const PRICE_SOURCES = new Set([
  "marketdata.refresh_quotes",
  "marketdata.refresh_fx",
  "tinvest.refresh_quotes",
]);

export function StaleSourcesNotice({ canOpenSettings }: { canOpenSettings: boolean }) {
  const { t } = useTranslation();
  const sources = useDataSources();
  const stale = (sources.data ?? []).filter((s) => s.stale && PRICE_SOURCES.has(s.kind));
  if (stale.length === 0) return null;
  return (
    <Alert data-testid="stale-sources">
      <AlertDescription>
        {t("dataSources.notice", { sources: stale.map((s) => sourceName(t, s.kind)).join(", ") })}{" "}
        {canOpenSettings && (
          <Link to="/settings" className="underline">
            {t("dataSources.noticeLink")}
          </Link>
        )}
      </AlertDescription>
    </Alert>
  );
}
