import { useTranslation } from "react-i18next";
import type { Operation } from "@/api/operations";
import { formatMinor, formatPriceIn } from "@/lib/money";
import { formatDate } from "@/lib/dates";

type Withheld = NonNullable<Operation["withheld_abroad"]>;

// rate is the server's percentage with one decimal ("30.0"), shown the Russian way.
function formatRate(rate: string): string {
  return new Intl.NumberFormat("ru-RU", { minimumFractionDigits: 1, maximumFractionDigits: 1 }).format(
    Number(rate),
  );
}

// The tax a foreign dividend lost abroad (Р-14), under the row's type: an
// estimate marked ≈ with its arithmetic in the tooltip, or «unknown» with the
// reason; and the tax the broker reported as its own row on the payment.
export function WithheldAbroadNote({ withheld }: { withheld: Withheld }) {
  const { t } = useTranslation();
  const money = (minor: number, currency = withheld.currency) => formatMinor(minor, currency);
  const brokerTax = withheld.broker_tax.map((x) => money(x.amount_minor, x.currency)).join(", ");

  return (
    <div className="max-w-64 text-xs whitespace-normal text-muted-foreground" data-testid="operation-withheld">
      {withheld.state === "estimated" && withheld.tax_minor != null && withheld.gross_minor != null && (
        <div
          data-testid="operation-withheld-estimate"
          title={t("operations.withheldEstimateTitle", {
            perShare: withheld.per_share ? (formatPriceIn(withheld.per_share, withheld.currency) ?? withheld.per_share) : "",
            shares: withheld.shares ? Number(withheld.shares).toLocaleString("ru-RU") : "",
            date: withheld.record_date ? formatDate(withheld.record_date) : "",
            gross: money(withheld.gross_minor),
            received: money(withheld.received_minor),
          })}
        >
          {t("operations.withheldEstimated", {
            tax: money(withheld.tax_minor),
            rate: formatRate(withheld.rate_percent ?? "0"),
          })}
          {withheld.tax_in_base && (
            <>
              {", "}
              {t("operations.withheldInBase", {
                amount: money(withheld.tax_in_base.amount_minor, withheld.tax_in_base.currency),
              })}
            </>
          )}
        </div>
      )}
      {withheld.state === "unknown" && (
        <div
          data-testid="operation-withheld-unknown"
          title={withheld.unknown_reason ? t(`operations.withheldReason.${withheld.unknown_reason}`) : undefined}
        >
          {t("operations.withheldUnknown")}
        </div>
      )}
      {brokerTax !== "" && (
        <div data-testid="operation-withheld-broker">{t("operations.brokerTax", { amount: brokerTax })}</div>
      )}
    </div>
  );
}
