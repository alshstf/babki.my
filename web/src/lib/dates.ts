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

// EARLIEST_OPERATION_DATE is the oldest date the server accepts on an
// operation, copied here so a date field refuses at the keystroke instead of
// after a round trip — the same arrangement money.ts holds for the amount cap.
//
// It exists as a typo guard and not as a rule from anywhere: a four-digit year
// with a fumbled leading digit (1026 for 2026) is one keystroke, and the
// journal's queue is ordered by acquisition date, so such a row lands at the
// FRONT of it and the next sale releases it first. Nothing on any screen would
// remark on the date — centuries old is an ordinary date to a comparison — so
// the only place it can be noticed is where it is typed, and where it is
// written. See minOccurredOn in internal/operation, which is the rule; this is
// a copy of it, tied to it by
// TestTheContractAndTheDateFieldsStateTheFloorTheServerEnforces.
//
// The four dialogs that write an operation carry it as the `min` of their date
// input, beside the `max` of localToday() that was already there. The
// balance dialog deliberately does not: a balance mark has no floor to copy,
// because a mark dated centuries ago is one the latest mark still wins over —
// a visible mistake in a row that can be retyped, not a wrong number nobody is
// told about.
export const EARLIEST_OPERATION_DATE = "1900-01-01";

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

// formatDateTime renders an instant — what every `format: date-time` field on
// the wire carries, sent by the server in UTC — as a short ru-RU date and time
// ON THE READER'S OWN CLOCK: an instant of 09:15 UTC reads "04.08.2026, 12:15"
// in Moscow. The shift is the point of the function and not an accident of it:
// these instants are shown to a person who is deciding whether a sync happened
// recently, and UTC would be that judgement made against somebody else's clock.
//
// Unlike formatDate above, which pins UTC deliberately: there the input is a
// calendar date with no time in it at all, and a timezone would move it to the
// wrong day.
//
// Anything Date cannot parse — an empty string, prose, a malformed instant —
// returns "" rather than the useless "Invalid Date", so a caller can drop the
// whole phrase instead of printing half of one.
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
