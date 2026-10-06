// localToday is the user's local date as YYYY-MM-DD; toISOString would give
// the UTC date, yesterday east of UTC after local midnight.
export function localToday(): string {
  const d = new Date();
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

// EARLIEST_OPERATION_DATE is the server's oldest accepted operation date,
// generated from it (api/constants.gen.ts) so a date field refuses at the
// keystroke; a typo guard (1026 for 2026 would land at the front of the queue).
// The four operation dialogs use it as `min`; the balance dialog does not: a
// mistyped mark is visible and the latest mark wins anyway.
export { EARLIEST_OPERATION_DATE } from "@/api/constants.gen";

// formatDate renders "YYYY-MM-DD" as short ru-RU ("20.07.2026"); malformed
// input gives "", so callers can drop the phrase.
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

// formatDateTime renders a UTC instant as a ru-RU date and time on the reader's
// own clock (09:15 UTC reads "04.08.2026, 12:15" in Moscow): it is judged as "did a
// sync happen recently". formatDate pins UTC because a calendar date has no time.
// Unparseable input gives "".
export function formatDateTime(iso: string): string {
  if (typeof iso !== "string") return "";
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  return new Intl.DateTimeFormat("ru-RU", {
    day: "2-digit",
    month: "2-digit",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(at);
}

// isRecent reports whether `iso` is `today` or the day before; `today` is passed
// in (normally localToday()) so tests need no clock.
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
