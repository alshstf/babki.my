import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useCapital, type CapitalPoint } from "@/api/capital";
import { formatMinorCompact } from "@/lib/money";
import { formatDate } from "@/lib/dates";

const WIDTH = 640;
const HEIGHT = 180;
const PAD = { top: 12, right: 12, bottom: 22, left: 12 };

// A year back from today, from the first of that month.
function yearAgo(): string {
  const d = new Date();
  return new Date(Date.UTC(d.getUTCFullYear() - 1, d.getUTCMonth(), 1)).toISOString().slice(0, 10);
}

// The family's worth at each month's end over the last year, as the total
// counts it (see GET /api/v1/capital). A month in which something could not be
// valued is drawn hollow, and says so on hover: its figure is short by what
// was left out.
export function CapitalChart() {
  const { t } = useTranslation();
  const capital = useCapital(yearAgo());
  const points = capital.data?.points ?? [];
  if (points.length < 2) return null;
  const currency = capital.data?.currency ?? "";
  const values = points.map((p) => p.total_minor);
  const low = Math.min(0, ...values);
  const high = Math.max(...values);
  const span = high - low || 1;
  const x = (i: number) => PAD.left + (i * (WIDTH - PAD.left - PAD.right)) / (points.length - 1);
  const y = (v: number) => PAD.top + ((high - v) * (HEIGHT - PAD.top - PAD.bottom)) / span;
  const line = points.map((p, i) => `${i === 0 ? "M" : "L"}${x(i)},${y(p.total_minor)}`).join(" ");
  const area = `${line} L${x(points.length - 1)},${y(low)} L${x(0)},${y(low)} Z`;
  const incomplete = points.some((p) => !p.complete);
  const title = (p: CapitalPoint) =>
    `${formatDate(p.day)}: ${formatMinorCompact(p.total_minor, currency)}` +
    (p.complete ? "" : ` — ${t("capital.incompletePoint")}`);

  return (
    <Card size="sm">
      <CardHeader className="pb-1">
        <CardTitle className="text-sm font-medium text-muted-foreground">
          {t("capital.title")}
        </CardTitle>
      </CardHeader>
      <CardContent className="grid gap-1">
        <svg
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="h-44 w-full"
          role="img"
          aria-label={t("capital.title")}
          data-testid="capital-chart"
        >
          <path d={area} className="fill-primary/10" />
          <path d={line} className="fill-none stroke-primary" strokeWidth={2} />
          {points.map((p, i) => (
            <circle
              key={p.day}
              cx={x(i)}
              cy={y(p.total_minor)}
              r={3.5}
              className={p.complete ? "fill-primary" : "fill-background stroke-amber-600"}
              strokeWidth={p.complete ? 0 : 2}
              data-testid={`capital-point-${p.day}`}
            >
              <title>{title(p)}</title>
            </circle>
          ))}
          <text x={x(0)} y={HEIGHT - 4} className="fill-muted-foreground text-[11px]">
            {formatDate(points[0].day)}
          </text>
          <text x={x(points.length - 1)} y={HEIGHT - 4} textAnchor="end" className="fill-muted-foreground text-[11px]">
            {formatDate(points[points.length - 1].day)}
          </text>
        </svg>
        <div className="flex justify-between text-xs text-muted-foreground">
          <span>{formatMinorCompact(points[0].total_minor, currency)}</span>
          <span>{formatMinorCompact(points[points.length - 1].total_minor, currency)}</span>
        </div>
        {incomplete && (
          <div className="text-xs text-amber-700" data-testid="capital-incomplete">
            {t("capital.incomplete")}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
