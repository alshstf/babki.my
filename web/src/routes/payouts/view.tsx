import { useTranslation } from "react-i18next";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { formatMinor } from "@/lib/money";
import type { Payout } from "@/api/payouts";
import { netSource, type PayoutView } from "@/lib/payout-view";

export function PayoutViewSelect({ view, onChange, className }: { view: PayoutView; onChange: (v: PayoutView) => void; className?: string }) {
  const { t } = useTranslation();
  return (
    <Select value={view} onValueChange={(v) => onChange(v === "gross" ? "gross" : "net")}>
      <SelectTrigger className={className ?? "w-48"} aria-label={t("payouts.view")}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="net">{t("payouts.views.net")}</SelectItem>
        <SelectItem value="gross">{t("payouts.views.gross")}</SelectItem>
      </SelectContent>
    </Select>
  );
}

// PayoutAmount is a payout's sum in the chosen view: what reaches the account
// with where that comes from, or the amount before tax.
export function PayoutAmount({ p, view, base }: { p: Payout; view: PayoutView; base?: string }) {
  const { t } = useTranslation();
  const net = view === "net" && p.net_minor != null;
  const minor = net ? p.net_minor : p.amount_minor;
  const inBase = net ? p.net_in_base_minor : p.in_base_minor;
  if (minor == null) return <>{p.kind === "offer" ? t("payouts.offerNote") : t("payouts.notSet")}</>;
  return (
    <>
      {formatMinor(minor, p.currency)}
      {base && p.currency !== base && inBase != null && <div className="text-xs text-muted-foreground">≈ {formatMinor(inBase, base)}</div>}
      {net && (
        <div className="min-w-32 whitespace-normal text-xs text-muted-foreground" data-testid="payout-net-by">
          {netSource(t, p)}
          {p.net_minor !== p.amount_minor && p.amount_minor != null && ` · ${t("payouts.before", { amount: formatMinor(p.amount_minor, p.currency) })}`}
        </div>
      )}
    </>
  );
}
