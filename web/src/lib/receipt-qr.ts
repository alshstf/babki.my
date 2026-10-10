// The QR code every Russian cash receipt carries (54-ФЗ) holds one line:
//
//   t=20261010T1230&s=1234.50&fn=7380440700000000&i=12345&fp=1234567890&n=1
//
// t — when, to the minute (seconds sometimes); s — the total in roubles; fn —
// the fiscal drive; i — the document's number on it; fp — its fiscal sign; n —
// what the receipt is: 1 a purchase, 2 its refund, 3 the seller paying out
// (scrap bought from you), 4 the refund of that. fn, i and fp name the receipt
// for good: what the tax service's check asks for, and what tells a receipt
// scanned twice.

export type ReceiptKind = "purchase" | "refund" | "payout" | "payoutRefund";

export interface Receipt {
  amountMinor: number;
  // YYYY-MM-DD and HH:MM, the till's local time as printed.
  date: string;
  time: string;
  kind: ReceiptKind;
  fn: string;
  fd: string;
  fp: string;
}

const KINDS: Record<string, ReceiptKind> = { "1": "purchase", "2": "refund", "3": "payout", "4": "payoutRefund" };

// Whether the money comes in to the family: a refund of a purchase, or the
// seller paying out.
export function receiptIsIncoming(kind: ReceiptKind): boolean {
  return kind === "refund" || kind === "payout";
}

// parseReceiptQr reads the line, or says it is not a receipt's: any field
// missing or malformed is null, never a half-filled form.
export function parseReceiptQr(text: string): Receipt | null {
  const params = new URLSearchParams(text.trim());
  const t = params.get("t") ?? "";
  const s = params.get("s") ?? "";
  const fn = params.get("fn") ?? "";
  const fd = params.get("i") ?? "";
  const fp = params.get("fp") ?? "";
  const kind = KINDS[params.get("n") ?? ""];
  const when = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})?$/.exec(t);
  const sum = /^(\d{1,12})(?:[.,](\d{1,2}))?$/.exec(s);
  if (!when || !sum || !kind || !/^\d+$/.test(fn) || !/^\d+$/.test(fd) || !/^\d+$/.test(fp)) return null;
  const [, y, mo, d, h, mi] = when;
  const month = Number(mo);
  const day = Number(d);
  if (month < 1 || month > 12 || day < 1 || day > 31 || Number(h) > 23 || Number(mi) > 59) return null;
  const amountMinor = Number(sum[1]) * 100 + Number((sum[2] ?? "0").padEnd(2, "0"));
  if (amountMinor <= 0) return null;
  return { amountMinor, date: `${y}-${mo}-${d}`, time: `${h}:${mi}`, kind, fn, fd, fp };
}
