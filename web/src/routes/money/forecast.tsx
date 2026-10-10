import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { useForecast, type Forecast } from "@/api/forecast";
import { formatMinor, formatMinorCompact, signClass } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { cn } from "@/lib/utils";

const HORIZONS = [30, 90, 180];
const SOONEST = 6;

// ForecastCard is the family's money to live on ahead (household stage 4): how
// much can be spent before the next salary, the line of the money day by day
// and the payments coming. Nothing while there is no account to count.
export function ForecastCard() {
  const { t } = useTranslation();
  const [days, setDays] = useState(90);
  const forecast = useForecast(days);
  const f = forecast.data;
  const horizonLabel: Record<number, string> = {
    30: t("forecast.horizons.d30"),
    90: t("forecast.horizons.d90"),
    180: t("forecast.horizons.d180"),
  };
  if (!f || f.accounts_counted === 0) return null;
  const c = f.base_currency;
  const free = f.free_until_income_minor;
  return (
    <Card data-testid="money-forecast">
      <CardHeader className="flex flex-row flex-wrap items-start justify-between gap-2">
        <div className="grid gap-1">
          <CardTitle>{t("forecast.title")}</CardTitle>
          <p className="text-sm text-muted-foreground">{t("forecast.hint")}</p>
        </div>
        <Select value={String(days)} onValueChange={(v) => setDays(Number(v))}>
          <SelectTrigger className="w-36" aria-label={t("forecast.horizon")}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {HORIZONS.map((h) => (
              <SelectItem key={h} value={String(h)}>
                {horizonLabel[h]}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </CardHeader>
      {/* One column that may be narrower than a payee's long name: the name is
          cut with an ellipsis rather than widening the card past the screen. */}
      <CardContent className="grid grid-cols-1 gap-3">
        {f.next_income && free != null ? (
          <div data-testid="forecast-free">
            <div className="text-sm text-muted-foreground">
              {t("forecast.untilIncome", { date: formatDate(f.next_income.on), name: f.next_income.name })}
            </div>
            <div className={cn("text-2xl font-semibold tabular-nums", free < 0 && "text-amber-700 dark:text-amber-400")}>
              {free < 0 ? t("forecast.short", { amount: formatMinor(-free, c) }) : formatMinor(free, c)}
            </div>
          </div>
        ) : (
          <div className="text-sm text-muted-foreground" data-testid="forecast-no-income">
            {t("forecast.noIncome")}
          </div>
        )}
        <div className="text-sm text-muted-foreground">
          {t("forecast.now", { amount: formatMinor(f.start_minor, c), count: f.accounts_counted })}
          {" · "}
          {t("forecast.lowest", { amount: formatMinor(f.lowest.balance_minor, c), date: formatDate(f.lowest.on) })}
        </div>
        {f.events.length > 0 && <ForecastChart forecast={f} />}
        {f.events.length > 0 ? (
          <ul className="grid grid-cols-1 gap-1 text-sm" data-testid="forecast-events">
            {f.events.slice(0, SOONEST).map((e) => (
              <li key={`${e.on}-${e.name}-${e.kind}-${e.amount_minor}`} className="flex justify-between gap-3">
                <span className={cn("min-w-0 truncate", e.overdue && "text-amber-700 dark:text-amber-400")}>
                  {e.overdue ? t("forecast.expectedToday") : formatDate(e.on)} · {e.name}
                  {e.kind === "loan" && <span className="text-muted-foreground"> · {t("forecast.loan")}</span>}
                </span>
                <span className={cn("shrink-0 tabular-nums", signClass(e.in_base_minor))}>
                  {formatMinor(e.in_base_minor, c)}
                </span>
              </li>
            ))}
          </ul>
        ) : (
          <div className="text-sm text-muted-foreground">{t("forecast.noEvents")}</div>
        )}
        {f.missing_rates.length > 0 && (
          <div className="text-xs text-amber-700">{t("forecast.missingRates", { currencies: f.missing_rates.join(", ") })}</div>
        )}
      </CardContent>
    </Card>
  );
}

// ForecastChart draws the money day by day across the card's width, the zero
// line when it dips below, and a dot on each day something comes or goes. The
// line stretches with the card; the dots sit over it as their own elements so
// they stay round.
function ForecastChart({ forecast }: { forecast: Forecast }) {
  const { t } = useTranslation();
  const points = forecast.series;
  const c = forecast.base_currency;
  const values = points.map((p) => p.balance_minor);
  const low = Math.min(0, ...values);
  const high = Math.max(0, ...values);
  const span = high - low || 1;
  // In hundredths of the box: the svg's viewBox is 100 by 100, stretched.
  const x = (i: number) => (i * 100) / Math.max(1, points.length - 1);
  const y = (v: number) => 4 + ((high - v) * 92) / span;
  const line = points.map((p, i) => `${i === 0 ? "M" : "L"}${x(i)},${y(p.balance_minor)}`).join(" ");
  const index = new Map(points.map((p, i) => [p.on, i]));
  const byDay = new Map<string, string[]>();
  for (const e of forecast.events) {
    byDay.set(e.on, [...(byDay.get(e.on) ?? []), `${e.name} ${formatMinor(e.in_base_minor, c)}`]);
  }
  return (
    <div className="grid gap-1">
      <div className="relative h-40 w-full" role="img" aria-label={t("forecast.chart")} data-testid="forecast-chart">
        <svg viewBox="0 0 100 100" preserveAspectRatio="none" className="absolute inset-0 h-full w-full overflow-visible">
          {/* Zero: below it the money runs out. */}
          <line
            x1={0}
            x2={100}
            y1={y(0)}
            y2={y(0)}
            className={low < 0 ? "stroke-amber-600/60" : "stroke-muted-foreground/30"}
            strokeDasharray="4 4"
            vectorEffect="non-scaling-stroke"
          />
          <path d={line} className="fill-none stroke-primary" strokeWidth={2} vectorEffect="non-scaling-stroke" />
        </svg>
        {[...byDay.entries()].map(([on, items]) => {
          const i = index.get(on);
          if (i === undefined) return null;
          const v = points[i].balance_minor;
          return (
            <span
              key={on}
              className={cn(
                "absolute size-2 -translate-x-1/2 -translate-y-1/2 rounded-full",
                v < 0 ? "bg-amber-600" : "bg-primary",
              )}
              style={{ left: `${x(i)}%`, top: `${y(v)}%` }}
              title={`${formatDate(on)}: ${formatMinorCompact(v, c)}\n${items.join("\n")}`}
            />
          );
        })}
      </div>
      <div className="flex justify-between text-xs text-muted-foreground">
        <span>{formatDate(points[0].on)}</span>
        <span>{formatDate(points[points.length - 1].on)}</span>
      </div>
    </div>
  );
}
