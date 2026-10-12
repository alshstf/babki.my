import { useState } from "react";
import type { Payout } from "@/api/payouts";

// PayoutView is whether payouts show what reaches the account or the amount
// before tax (decision Р-38). The choice is the browser's, shared by the
// payouts page and a paper's block; «what reaches the account» unless picked
// otherwise.
export type PayoutView = "net" | "gross";

const KEY = "babki.payoutView";

function stored(): PayoutView {
  try {
    return localStorage.getItem(KEY) === "gross" ? "gross" : "net";
  } catch {
    return "net";
  }
}

export function usePayoutView(): [PayoutView, (v: PayoutView) => void] {
  const [view, setView] = useState<PayoutView>(stored);
  const pick = (v: PayoutView) => {
    setView(v);
    try {
      localStorage.setItem(KEY, v);
    } catch {
      // Remembering is a convenience: without storage the choice lasts the visit.
    }
  };
  return [view, pick];
}

// netSource says where «what reaches the account» comes from: as last time,
// by the rule, or the amount before tax when no tax is known.
export function netSource(t: (key: string) => string, p: Payout): string {
  if (p.net_by === "history") return t("payouts.netBy.history");
  if (p.net_by === "rule") return p.kind === "amortization" || p.kind === "redemption" ? t("payouts.netBy.face") : t("payouts.netBy.rule");
  return t("payouts.netBy.none");
}
