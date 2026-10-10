// The periods offered, each worked out from today in the browser's own zone.
export const PERIODS = ["thisMonth", "lastMonth", "last3", "last6", "thisYear", "lastYear"] as const;
export type Period = (typeof PERIODS)[number];

const iso = (d: Date) =>
  `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;

// periodDays turns a period into its first and last day; a period that is
// still running ends today.
export function periodDays(period: Period, today: Date = new Date()): { from: string; to: string } {
  const y = today.getFullYear();
  const m = today.getMonth();
  switch (period) {
    case "thisMonth":
      return { from: iso(new Date(y, m, 1)), to: iso(today) };
    case "lastMonth":
      return { from: iso(new Date(y, m - 1, 1)), to: iso(new Date(y, m, 0)) };
    case "last3":
      return { from: iso(new Date(y, m - 2, 1)), to: iso(today) };
    case "last6":
      return { from: iso(new Date(y, m - 5, 1)), to: iso(today) };
    case "thisYear":
      return { from: iso(new Date(y, 0, 1)), to: iso(today) };
    case "lastYear":
      return { from: iso(new Date(y - 1, 0, 1)), to: iso(new Date(y - 1, 11, 31)) };
  }
}
