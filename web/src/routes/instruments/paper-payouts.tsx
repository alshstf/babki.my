import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Badge } from "@/components/ui/badge";
import { usePayouts } from "@/api/payouts";
import { formatDate } from "@/lib/dates";
import { PayoutAmount, PayoutViewSelect } from "@/routes/payouts/view";
import { usePayoutView } from "@/lib/payout-view";

// PaperPayouts is what this paper will pay over the next year to the accounts
// holding it today; nothing at all when nothing is announced.
export function PaperPayouts({
  instrumentId,
  accountName,
}: {
  instrumentId: string;
  accountName: (id: string) => string | undefined;
}) {
  const { t } = useTranslation();
  const forecast = usePayouts(12, undefined, instrumentId);
  const [view, setView] = usePayoutView();
  const data = forecast.data;
  if (!data || data.payouts.length === 0) return null;
  return (
    <div className="grid gap-2" data-testid="paper-payouts">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="text-lg font-semibold">{t("payouts.paperTitle")}</h2>
        <div className="flex items-center gap-3">
          <PayoutViewSelect view={view} onChange={setView} className="h-8 w-44" />
          <Link to="/payouts" className="text-sm text-muted-foreground underline underline-offset-2">
            {t("payouts.allPayouts")}
          </Link>
        </div>
      </div>
      <ul className="grid gap-1 text-sm">
        {data.payouts.map((p, i) => (
          <li key={`${p.account_id}-${p.kind}-${p.on}-${i}`} className="flex flex-wrap items-center gap-x-3 gap-y-1" data-testid="paper-payout">
            <span className="w-24 tabular-nums">{formatDate(p.on)}</span>
            <Badge variant={p.kind === "offer" ? "outline" : "secondary"}>{t(`payouts.kinds.${p.kind}`)}</Badge>
            <span className="text-muted-foreground">{accountName(p.account_id) ?? "—"}</span>
            <span className="ml-auto text-right tabular-nums">
              <PayoutAmount p={p} view={view} />
            </span>
          </li>
        ))}
      </ul>
      <p className="text-xs text-muted-foreground">{view === "net" ? t("payouts.paperHintNet") : t("payouts.paperHint")}</p>
    </div>
  );
}
