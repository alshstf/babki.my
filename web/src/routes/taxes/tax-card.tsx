import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useTaxYear } from "@/api/tax-year";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";

// TaxCard is the year's tax in a line on «Деньги» (decision Р-42): what is
// left to pay by return, or to get back, and the way to the page.
export function TaxCard() {
  const { t } = useTranslation();
  const [year] = useState(() => new Date().getFullYear());
  const q = useTaxYear(year);
  const d = q.data;
  if (!d || !d.supported || d.people.length === 0) return null;
  return (
    <Card data-testid="tax-card">
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{t("taxes.cardTitle", { year: d.year })}</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-wrap items-baseline justify-between gap-2 text-sm">
        <span>
          {d.to_pay_minor > 0
            ? t("taxes.cardToPay", { amount: formatMinor(d.to_pay_minor, d.currency), date: formatDate(d.pay_by) })
            : d.refund_minor > 0
              ? t("taxes.cardRefund", { amount: formatMinor(d.refund_minor, d.currency) })
              : t("taxes.nothingToPay")}
        </span>
        <Link to="/taxes" className="text-muted-foreground underline underline-offset-2">
          {t("taxes.cardMore")}
        </Link>
      </CardContent>
    </Card>
  );
}
