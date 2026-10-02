import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { useAccountReturn, useFamilyReturn, useInstrumentReturn, type PeriodReturn } from "@/api/returns";
import { formatMinor, signClass } from "@/lib/money";
import { localToday } from "@/lib/dates";
import { cn } from "@/lib/utils";

type Period = "year" | "ytd" | "all";

// The day before the period: the server reckons a period from the end of its
// `from` day.
function fromFor(period: Period, today: string): string {
  const [y, m, d] = today.split("-").map(Number);
  switch (period) {
    case "year":
      return new Date(Date.UTC(y - 1, m - 1, d)).toISOString().slice(0, 10);
    case "ytd":
      return `${y - 1}-12-31`;
    case "all":
      return "2000-01-01";
  }
}

// A rate published as a decimal fraction, as a percentage for reading.
function percent(rate: string): string {
  return new Intl.NumberFormat("ru-RU", {
    style: "percent",
    maximumFractionDigits: 1,
    signDisplay: "exceptZero",
  }).format(Number(rate));
}

// What the account earned over a period, by its journal: the profit (worth at
// the end, less worth at the start, less money put in net) and the
// money-weighted annual rate — or, when something in the period could not be
// valued, why there is no figure. Everything is the server's; this only words
// it.
export function AccountReturn({ accountId }: { accountId: string }) {
  const [period, setPeriod] = useState<Period>("year");
  const today = localToday();
  const result = useAccountReturn(accountId, fromFor(period, today), today);
  return <ReturnLine r={result.data} period={period} onPeriod={setPeriod} />;
}

// The same over the family's brokerage accounts kept by their journal, with
// how many there are; nothing when there are none.
export function FamilyReturnLine() {
  const { t } = useTranslation();
  const [period, setPeriod] = useState<Period>("year");
  const today = localToday();
  const result = useFamilyReturn(fromFor(period, today), today);
  if (!result.data || result.data.accounts === 0) return null;
  return (
    <div className="grid gap-1">
      <div className="text-sm font-medium text-muted-foreground">
        {t("accountReturn.family", { count: result.data.accounts })}
      </div>
      <ReturnLine r={result.data} period={period} onPeriod={setPeriod} />
    </div>
  );
}

// The same for one paper across the family's accounts: what went into it, what
// came out, and what it is worth.
export function PaperReturn({ instrumentId }: { instrumentId: string }) {
  const [period, setPeriod] = useState<Period>("year");
  const today = localToday();
  const result = useInstrumentReturn(instrumentId, fromFor(period, today), today);
  return <ReturnLine r={result.data} period={period} onPeriod={setPeriod} />;
}

function ReturnLine({
  r,
  period,
  onPeriod,
}: {
  r: PeriodReturn | undefined;
  period: Period;
  onPeriod: (p: Period) => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="grid gap-1" data-testid="account-return">
      <div className="flex flex-wrap items-center gap-1">
        {(["year", "ytd", "all"] as const).map((p) => (
          <Button
            key={p}
            size="sm"
            variant={p === period ? "secondary" : "ghost"}
            className="h-7 px-2 text-xs"
            onClick={() => onPeriod(p)}
          >
            {t(`accountReturn.periods.${p}`)}
          </Button>
        ))}
      </div>
      {/* A period valued only in part is not shown in part: a holding left out
          at the end and not at the start reads as a loss that never happened. */}
      {r && r.complete && (
        <div className="flex flex-wrap items-baseline gap-x-3 text-sm">
          <span>
            {t("accountReturn.profit")}{" "}
            <span className={cn("font-semibold tabular-nums", signClass(r.profit_minor))} data-testid="account-return-profit">
              {formatMinor(r.profit_minor, r.currency)}
            </span>
          </span>
          {r.annual_rate !== null && r.annual_rate !== undefined && (
            <span title={t("accountReturn.rateHint")}>
              {t("accountReturn.rate")}{" "}
              <span className="font-semibold tabular-nums" data-testid="account-return-rate">
                {percent(r.annual_rate)}
              </span>
            </span>
          )}
          <span className="text-xs text-muted-foreground">
            {t("accountReturn.contributions", { amount: formatMinor(r.contributions_minor, r.currency) })}
          </span>
        </div>
      )}
      {r && !r.complete && (
        <div className="text-xs text-amber-700" data-testid="account-return-incomplete">
          {t("accountReturn.incomplete")}
        </div>
      )}
    </div>
  );
}
