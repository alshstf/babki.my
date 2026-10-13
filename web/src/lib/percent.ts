// A percent as the API writes it ("35.00"), as people read it ("35 %").
export function percent(s: string): string {
  return `${Number(s).toLocaleString("ru-RU", { maximumFractionDigits: 2 })} %`;
}

// hundredths reads a percent typed with a comma or a point, to hundredths;
// null when it is not one, empty is no target.
export function hundredths(s: string): number | null {
  const m = /^(\d{1,3})(?:[.,](\d{1,2}))?$/.exec(s.trim());
  if (!m) return null;
  const v = Number(m[1]) * 100 + Number((m[2] ?? "").padEnd(2, "0"));
  return v <= 10_000 ? v : null;
}
