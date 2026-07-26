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
