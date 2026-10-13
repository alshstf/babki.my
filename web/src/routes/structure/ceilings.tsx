import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useAllocation, useSetTargets, type AllocationRow } from "@/api/allocation";
import { useSession } from "@/api/session";
import { useIntermediaries } from "@/api/intermediaries";
import { countryName } from "@/lib/country";
import { formatMinorCompact } from "@/lib/money";
import { hundredths, percent } from "@/lib/percent";
import { sectorName } from "@/lib/sectors";

type Dimension = "country" | "sector" | "broker" | "bank";

// CeilingsCard is the papers against the family's ceilings by country of risk
// and by sector (decision Р-46): «no more than N %», each on its own, for the
// record — the suggestions stay by kind of asset.
export function CeilingsCard({ valuation }: { valuation: "liquid" | "full" }) {
  const { t } = useTranslation();
  const allocation = useAllocation(valuation);
  const { data: session } = useSession();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const [editing, setEditing] = useState<Dimension | null>(null);
  const intermediaries = useIntermediaries();
  const a = allocation.data;
  if (!a || (a.papers_minor === 0 && a.at_brokers_minor === 0 && a.at_banks_minor === 0)) return null;
  const c = a.base_currency;
  const country = (k: string) => (k === "-" ? t("structure.noIsin") : countryName(k));
  const sector = (k: string) => sectorName(t, k);
  const holder = (k: string) => (k === "-" ? t("structure.unknownHolder") : (intermediaries.data?.find((i) => i.id === k)?.name ?? k));
  const rowsOf = (d: Dimension) => (d === "country" ? a.countries : d === "sector" ? a.sectors : d === "broker" ? a.brokers : a.banks);
  const nameOf = (d: Dimension) => (d === "country" ? country : d === "sector" ? sector : holder);
  return (
    <Card className="min-w-0" data-testid="ceilings">
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{t("ceilings.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("ceilings.hint", { papers: formatMinorCompact(a.papers_minor, c) })}</p>
      </CardHeader>
      <CardContent className="grid gap-4">
        <Section title={t("ceilings.countries")} rows={a.countries} currency={c} name={country} testId="ceilings-countries"
          onEdit={canEdit ? () => setEditing("country") : undefined} />
        <Section title={t("ceilings.sectors")} rows={a.sectors} currency={c} name={sector} testId="ceilings-sectors"
          onEdit={canEdit ? () => setEditing("sector") : undefined} />
        {a.brokers.length > 0 && (
          <Section title={t("ceilings.brokers")} rows={a.brokers} currency={c} name={holder} testId="ceilings-brokers"
            onEdit={canEdit ? () => setEditing("broker") : undefined} />
        )}
        {a.banks.length > 0 && (
          <Section title={t("ceilings.banks")} rows={a.banks} currency={c} name={holder} testId="ceilings-banks"
            onEdit={canEdit ? () => setEditing("bank") : undefined} />
        )}
      </CardContent>
      {editing && (
        <CeilingsDialog dimension={editing} rows={rowsOf(editing)} name={nameOf(editing)} onClose={() => setEditing(null)} />
      )}
    </Card>
  );
}

function Section({ title, rows, currency, name, testId, onEdit }: {
  title: string;
  rows: AllocationRow[];
  currency: string;
  name: (key: string) => string;
  testId: string;
  onEdit?: () => void;
}) {
  const { t } = useTranslation();
  const any = rows.some((r) => r.target !== null);
  return (
    <div className="grid gap-2" data-testid={testId}>
      <p className="text-sm font-medium">{title}</p>
      {rows.map((r) => {
        const over = r.target !== null && (r.off_minor ?? 0) > 0;
        return (
          <div key={r.key} className="grid gap-1" data-testid="ceiling-row">
            <div className="flex items-baseline justify-between gap-3 text-sm">
              <span className="min-w-0 break-words">{name(r.key)}</span>
              <span className="shrink-0 tabular-nums">
                {percent(r.share)}
                {r.target !== null && <span className="text-muted-foreground"> · {t("ceilings.ceiling", { ceiling: percent(r.target) })}</span>}
              </span>
            </div>
            <div className="relative h-2 rounded bg-muted">
              <div className={over ? "h-full rounded bg-amber-500" : "h-full rounded bg-bar"} style={{ width: `${Math.min(Number(r.share), 100)}%` }} />
              {r.target !== null && (
                <div className="absolute -top-1 -bottom-1 w-0.5 bg-foreground" style={{ left: `${Math.min(Number(r.target), 100)}%` }} />
              )}
            </div>
            {over && <p className="text-xs text-amber-700 dark:text-amber-400">{t("ceilings.over", { amount: formatMinorCompact(r.off_minor ?? 0, currency) })}</p>}
          </div>
        );
      })}
      {onEdit && (
        <Button type="button" variant="outline" size="sm" className="justify-self-start" onClick={onEdit}>
          {any ? t("ceilings.edit") : t("ceilings.set")}
        </Button>
      )}
    </div>
  );
}

// CeilingsDialog states the ceilings of one dimension: a percent at each
// country or sector held, empty for none; they do not add up to anything.
function CeilingsDialog({ dimension, rows, name, onClose }: {
  dimension: Dimension;
  rows: AllocationRow[];
  name: (key: string) => string;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const save = useSetTargets();
  const keys = rows.map((r) => r.key).filter((k) => k !== "-");
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(keys.map((k) => [k, rows.find((r) => r.key === k)?.target?.replace(/\.00$/, "").replace(".", ",") ?? ""])),
  );
  const filled = keys.filter((k) => (values[k] ?? "").trim() !== "");
  const parsed = filled.map((k) => hundredths(values[k]));
  const valid = parsed.every((v) => v !== null);
  const submit = () =>
    save.mutate(
      { dimension, targets: filled.map((k, i) => ({ key: k, percent: ((parsed[i] ?? 0) / 100).toFixed(2) })) },
      { onSuccess: onClose },
    );
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-md" data-testid="ceilings-dialog">
        <DialogHeader>
          <DialogTitle>
            {dimension === "country"
              ? t("ceilings.dialogCountries")
              : dimension === "sector"
                ? t("ceilings.dialogSectors")
                : dimension === "broker"
                  ? t("ceilings.dialogBrokers")
                  : t("ceilings.dialogBanks")}
          </DialogTitle>
        </DialogHeader>
        <div className="grid gap-2">
          <p className="text-xs text-muted-foreground">{t("ceilings.dialogHint")}</p>
          {keys.map((k) => (
            <div key={k} className="grid grid-cols-[1fr_5.5rem] items-center gap-3">
              <Label htmlFor={`ceiling-${k}`} className="grid gap-0.5 font-normal">
                <span>{name(k)}</span>
                <span className="text-xs text-muted-foreground">{t("allocation.now", { share: percent(rows.find((r) => r.key === k)?.share ?? "0") })}</span>
              </Label>
              <Input
                id={`ceiling-${k}`}
                inputMode="decimal"
                placeholder="—"
                value={values[k] ?? ""}
                onChange={(e) => setValues({ ...values, [k]: e.target.value })}
                aria-invalid={(values[k] ?? "").trim() !== "" && hundredths(values[k]) === null}
              />
            </div>
          ))}
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("allocation.saveFailed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter className="gap-2">
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || save.isPending} onClick={submit}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
