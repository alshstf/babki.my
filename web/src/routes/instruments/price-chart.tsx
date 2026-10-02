import { useTranslation } from "react-i18next";
import type { DayPrice } from "@/api/instrument-page";
import { formatDate } from "@/lib/dates";
import { priceText } from "./price-text";

const WIDTH = 640;
const HEIGHT = 160;
const PAD = { top: 6, right: 0, bottom: 6, left: 0 };

// The paper's closing prices, a day each, as a line. Coordinates only are
// worked out here; every figure shown is the server's own, formatted.
export function PriceChart({ points, bond }: { points: DayPrice[]; bond: boolean }) {
  const { t } = useTranslation();
  if (points.length < 2) return null;
  const values = points.map((p) => Number(p.price));
  const low = Math.min(...values);
  const high = Math.max(...values);
  const span = high - low || 1;
  const x = (i: number) => PAD.left + (i * (WIDTH - PAD.left - PAD.right)) / (points.length - 1);
  const y = (v: number) => PAD.top + ((high - v) * (HEIGHT - PAD.top - PAD.bottom)) / span;
  const line = values.map((v, i) => `${i === 0 ? "M" : "L"}${x(i)},${y(v)}`).join(" ");
  const first = points[0];
  const last = points[points.length - 1];
  return (
    <div className="grid gap-1">
      {/* Stretched to the card's width at a fixed height: the line is drawn
          in its own coordinates and keeps its stroke width, and the dates
          are text beside it rather than inside it, where the stretch would
          distort them. */}
      <svg
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
        preserveAspectRatio="none"
        className="h-40 w-full"
        role="img"
        aria-label={t("instrumentPage.chartLabel", {
          from: formatDate(first.on),
          to: formatDate(last.on),
          first: priceText(first, bond),
          last: priceText(last, bond),
        })}
        data-testid="price-chart"
      >
        <path d={line} className="fill-none stroke-primary" strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
      </svg>
      <div className="flex justify-between text-xs text-muted-foreground">
        <span>
          {formatDate(first.on)} · {priceText(first, bond)}
        </span>
        <span>
          {formatDate(last.on)} · {priceText(last, bond)}
        </span>
      </div>
    </div>
  );
}
