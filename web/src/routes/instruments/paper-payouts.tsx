import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Badge } from "@/components/ui/badge";
import { usePayouts } from "@/api/payouts";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";

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
  const data = forecast.data;
  if (!data || data.payouts.length === 0) return null;
  return (
    <div className="grid gap-2" data-testid="paper-payouts">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="text-lg font-semibold">{t("payouts.paperTitle")}</h2>
        <Link to="/payouts" className="text-sm text-muted-foreground underline underline-offset-2">
          {t("payouts.allPayouts")}
        </Link>
      </div>
      <ul className="grid gap-1 text-sm">
        {data.payouts.map((p, i) => (
          <li key={`${p.account_id}-${p.kind}-${p.on}-${i}`} className="flex flex-wrap items-center gap-x-3 gap-y-1" data-testid="paper-payout">
            <span className="w-24 tabular-nums">{formatDate(p.on)}</span>
            <Badge variant={p.kind === "offer" ? "outline" : "secondary"}>{t(`payouts.kinds.${p.kind}`)}</Badge>
            <span className="text-muted-foreground">{accountName(p.account_id) ?? "—"}</span>
            <span className="ml-auto tabular-nums">
              {p.amount_minor != null
                ? formatMinor(p.amount_minor, p.currency)
                : p.kind === "offer"
                  ? t("payouts.offerNote")
                  : t("payouts.notSet")}
            </span>
          </li>
        ))}
      </ul>
      <p className="text-xs text-muted-foreground">{t("payouts.paperHint")}</p>
    </div>
  );
}
