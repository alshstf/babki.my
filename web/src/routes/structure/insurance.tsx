import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useIntermediaries } from "@/api/intermediaries";
import { useMembers } from "@/api/members";
import type { Structure } from "@/api/structure";
import { formatMinorCompact } from "@/lib/money";

// InsuranceCard is what each person holds in each bank against deposit
// insurance (decision Р-47): what is over the limit is said, a bank the
// catalog does not know is not counted.
export function InsuranceCard({ data }: { data: Structure }) {
  const { t } = useTranslation();
  const members = useMembers();
  const intermediaries = useIntermediaries();
  if (data.insurance.length === 0) return null;
  const c = data.base_currency;
  const person = (id: string) => members.data?.find((m) => m.id === id)?.display_name ?? "";
  const bank = (id: string) => (id === "-" ? t("structure.unknownHolder") : (intermediaries.data?.find((i) => i.id === id)?.name ?? id));
  return (
    <Card className="min-w-0" data-testid="insurance">
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{t("insurance.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("insurance.hint", { limit: formatMinorCompact(data.insured_limit_minor, c) })}</p>
      </CardHeader>
      <CardContent className="grid gap-2">
        {data.insurance.map((r) => (
          <div key={`${r.user_id}-${r.bank}`} className="grid gap-0.5 text-sm" data-testid="insurance-row">
            <div className="flex flex-wrap items-baseline justify-between gap-x-3">
              <span className="min-w-0 break-words">{t("insurance.who", { person: person(r.user_id), bank: bank(r.bank) })}</span>
              <span className="shrink-0 tabular-nums">{formatMinorCompact(r.held_minor, c)}</span>
            </div>
            {!r.known ? (
              <p className="text-xs text-muted-foreground">{t("insurance.unknown")}</p>
            ) : r.over_minor > 0 ? (
              <p className="text-xs text-amber-700 dark:text-amber-400">
                {t("insurance.over", { over: formatMinorCompact(r.over_minor, c), insured: formatMinorCompact(r.insured_minor, c) })}
              </p>
            ) : (
              <p className="text-xs text-muted-foreground">{t("insurance.covered")}</p>
            )}
          </div>
        ))}
        <p className="text-xs text-muted-foreground">{t("insurance.notes")}</p>
      </CardContent>
    </Card>
  );
}
