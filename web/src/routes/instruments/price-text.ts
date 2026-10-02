import type { DayPrice } from "@/api/instrument-page";
import { formatPrice, formatPriceIn } from "@/lib/money";

// A price as it is read: money per unit in its currency, or for a bond a
// percentage of face value, as quotes are.
export function priceText(p: DayPrice, bond: boolean): string {
  if (bond) {
    const formatted = formatPrice(p.price);
    return formatted ? `${formatted} %` : p.price;
  }
  return formatPriceIn(p.price, p.currency) ?? `${p.price} ${p.currency}`;
}
