// localToday returns the user's local calendar date as YYYY-MM-DD.
// new Date().toISOString() would give the UTC date, which is yesterday
// for positive-offset timezones after local midnight.
export function localToday(): string {
  const d = new Date();
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

// formatDate renders an ISO "YYYY-MM-DD" date as short ru-RU ("20.07.2026").
// Malformed or empty input returns "" rather than the useless
// "Invalid Date" string, so callers can safely omit the whole context row.
export function formatDate(iso: string): string {
  if (typeof iso !== "string") return "";
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(iso);
  if (!match) return "";
  const [, yearStr, monthStr, dayStr] = match;
  const year = Number(yearStr);
  const month = Number(monthStr);
  const day = Number(dayStr);
  const date = new Date(Date.UTC(year, month - 1, day));
  // Date.UTC silently rolls over out-of-range components (e.g. month 13,
  // day 99) instead of failing, so round-trip the parts to reject those.
  if (
    date.getUTCFullYear() !== year ||
    date.getUTCMonth() !== month - 1 ||
    date.getUTCDate() !== day
  ) {
    return "";
  }
  return new Intl.DateTimeFormat("ru-RU", {
    day: "2-digit",
    month: "2-digit",
    year: "numeric",
    timeZone: "UTC",
  }).format(date);
}

// isRecent reports whether `iso` (YYYY-MM-DD) is `today` or the calendar day
// immediately before it. `today` is normally localToday() — passed in
// explicitly so callers (and tests) don't depend on wall-clock time.
export function isRecent(iso: string, today: string): boolean {
  if (iso === today) return true;
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(today);
  if (!match) return false;
  const [, y, m, d] = match;
  const t = new Date(Date.UTC(Number(y), Number(m) - 1, Number(d)));
  t.setUTCDate(t.getUTCDate() - 1);
  const yesterday = t.toISOString().slice(0, 10);
  return iso === yesterday;
}
