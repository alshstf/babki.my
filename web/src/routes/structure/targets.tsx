import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useAllocation, useSetTargets, type Allocation, type AllocationRow } from "@/api/allocation";
import { useSession } from "@/api/session";
import { formatMinorCompact } from "@/lib/money";
import { cn } from "@/lib/utils";

// The kinds targets are set for, in the server's order.
const KINDS = ["shares", "bonds", "funds", "foreign_currency", "metals", "deposits", "crypto", "other", "broker_cash"];

// A gap past this many points is worth looking at (the plan's threshold).
const NOTABLE_GAP = 5;

// kindName is a kind's words; a switch with literal keys for
// scripts/check-i18n.mjs.
function kindName(t: (key: string, v?: Record<string, unknown>) => string, key: string, base: string): string {
  switch (key) {
    case "shares":
      return t("allocation.kinds.shares");
    case "bonds":
      return t("allocation.kinds.bonds");
    case "funds":
      return t("allocation.kinds.funds");
    case "foreign_currency":
      return t("allocation.kinds.foreignCurrency");
    case "metals":
      return t("allocation.kinds.metals");
    case "deposits":
      return t("allocation.kinds.deposits");
    case "crypto":
      return t("allocation.kinds.crypto");
    case "broker_cash":
      return t("allocation.kinds.brokerCash", { currency: base });
    default:
      return t("allocation.kinds.other");
  }
}

// A currency's words: gold and the other metals held as money by name.
function currencyName(t: (key: string) => string, key: string): string {
  switch (key) {
    case "XAU":
      return t("allocation.metalCodes.gold");
    case "XAG":
      return t("allocation.metalCodes.silver");
    default:
      return key;
  }
}

// A percent as the API writes it ("35.00"), as people read it ("35 %").
function percent(s: string): string {
  return `${Number(s).toLocaleString("ru-RU", { maximumFractionDigits: 2 })} %`;
}

// TargetsCard is the worth against the family's target shares (decision
// Р-37): by kind of asset — what the suggestions will bring to the targets —
// and by currency, for the record. Each part's bar shows its share, the tick
// its target.
export function TargetsCard() {
  const { t } = useTranslation();
  const allocation = useAllocation();
  const { data: session } = useSession();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const [editing, setEditing] = useState<"kind" | "currency" | null>(null);
  const a = allocation.data;
  if (!a || (a.worth_minor === 0 && !canEdit)) return null;
  const c = a.base_currency;
  const kindTargets = a.kinds.some((r) => r.target !== null);
  const currencyTargets = a.currencies.some((r) => r.target !== null);
  return (
    <Card className="min-w-0" data-testid="allocation">
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{t("allocation.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("allocation.hint", { worth: formatMinorCompact(a.worth_minor, c) })}</p>
      </CardHeader>
      <CardContent className="grid gap-4">
        {!kindTargets && <p className="text-sm text-muted-foreground">{t("allocation.empty")}</p>}
        <Rows rows={a.kinds} currency={c} name={(k) => kindName(t, k, c)} testId="allocation-kinds" />
        {(a.everyday_minor > 0 || a.unsplit_minor > 0) && (
          <p className="text-xs text-muted-foreground" data-testid="allocation-outside">
            {[
              a.everyday_minor > 0 && t("allocation.everyday", { amount: formatMinorCompact(a.everyday_minor, c) }),
              a.unsplit_minor > 0 && t("allocation.unsplit", { amount: formatMinorCompact(a.unsplit_minor, c) }),
            ]
              .filter(Boolean)
              .join(" ")}
          </p>
        )}
        {canEdit && (
          <Button type="button" variant="outline" size="sm" className="justify-self-start" onClick={() => setEditing("kind")}>
            {kindTargets ? t("allocation.edit") : t("allocation.set")}
          </Button>
        )}
        <div className="grid gap-2 border-t pt-3">
          <p className="text-sm font-medium">{t("allocation.currenciesTitle")}</p>
          <p className="text-xs text-muted-foreground">{t("allocation.currenciesHint")}</p>
          <Rows rows={a.currencies} currency={c} name={(k) => currencyName(t, k)} testId="allocation-currencies" />
          {canEdit && (
            <Button type="button" variant="outline" size="sm" className="justify-self-start" onClick={() => setEditing("currency")}>
              {currencyTargets ? t("allocation.editCurrencies") : t("allocation.setCurrencies")}
            </Button>
          )}
        </div>
        {(a.unpriced > 0 || a.missing_rates.length > 0) && (
          <p className="text-xs text-amber-700 dark:text-amber-400">
            {a.unpriced > 0 && t("structure.unpriced", { count: a.unpriced })}{" "}
            {a.missing_rates.length > 0 && t("structure.missingRates", { currencies: a.missing_rates.join(", ") })}
          </p>
        )}
      </CardContent>
      {editing && <TargetsDialog allocation={a} dimension={editing} onClose={() => setEditing(null)} />}
    </Card>
  );
}

function Rows({ rows, currency, name, testId }: { rows: AllocationRow[]; currency: string; name: (key: string) => string; testId: string }) {
  const { t } = useTranslation();
  return (
    <div className="grid min-w-0 gap-3" data-testid={testId}>
      {rows.map((r) => {
        const gap = r.gap === null ? null : Number(r.gap);
        const notable = gap !== null && Math.abs(gap) > NOTABLE_GAP;
        return (
          <div key={r.key} className="grid min-w-0 gap-1" data-testid="allocation-row">
            <div className="flex flex-wrap items-baseline justify-between gap-x-3 text-sm">
              <span className="min-w-0">{name(r.key)}</span>
              <span className="ml-auto tabular-nums">
                {formatMinorCompact(r.minor, currency)} <span className="text-muted-foreground">{percent(r.share)}</span>
              </span>
            </div>
            <div className="relative h-2 rounded bg-muted">
              <div className="h-full rounded bg-emerald-500" style={{ width: `${Math.min(Number(r.share), 100)}%` }} />
              {r.target !== null && (
                <div
                  className="absolute -top-1 h-4 w-0.5 bg-foreground"
                  style={{ left: `calc(${Math.min(Number(r.target), 100)}% - 1px)` }}
                  title={t("allocation.targetTick", { target: percent(r.target) })}
                />
              )}
            </div>
            {r.target !== null && gap !== null && r.off_minor !== null && (
              <div className={cn("text-xs", notable ? "text-amber-700 dark:text-amber-400" : "text-muted-foreground")}>
                {gap === 0
                  ? t("allocation.onTarget", { target: percent(r.target) })
                  : gap > 0
                    ? t("allocation.above", { target: percent(r.target), gap: percent(String(gap)).replace(" %", ""), amount: formatMinorCompact(r.off_minor, currency) })
                    : t("allocation.below", { target: percent(r.target), gap: percent(String(-gap)).replace(" %", ""), amount: formatMinorCompact(-r.off_minor, currency) })}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

// hundredths reads a percent typed with a comma or a point, to hundredths;
// null when it is not one, empty is no target.
function hundredths(s: string): number | null {
  const m = /^(\d{1,3})(?:[.,](\d{1,2}))?$/.exec(s.trim());
  if (!m) return null;
  const v = Number(m[1]) * 100 + Number((m[2] ?? "").padEnd(2, "0"));
  return v <= 10_000 ? v : null;
}

// TargetsDialog states the targets of one dimension: a percent at each kind
// (or currency held, or one added), 100 in all; all empty takes them back.
function TargetsDialog({ allocation, dimension, onClose }: { allocation: Allocation; dimension: "kind" | "currency"; onClose: () => void }) {
  const { t } = useTranslation();
  const save = useSetTargets();
  const c = allocation.base_currency;
  const rows = dimension === "kind" ? allocation.kinds : allocation.currencies;
  const initialKeys = dimension === "kind" ? KINDS : rows.map((r) => r.key);
  const [keys, setKeys] = useState(initialKeys);
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(initialKeys.map((k) => [k, rows.find((r) => r.key === k)?.target?.replace(/\.00$/, "").replace(".", ",") ?? ""])),
  );
  const [added, setAdded] = useState("");
  const filled = keys.filter((k) => (values[k] ?? "").trim() !== "");
  const parsed = filled.map((k) => hundredths(values[k]));
  const sum = parsed.reduce<number>((s, v) => s + (v ?? 0), 0);
  const valid = parsed.every((v) => v !== null) && (filled.length === 0 || sum === 10_000);
  const name = (k: string) => (dimension === "kind" ? kindName(t, k, c) : currencyName(t, k));
  const submit = (clear: boolean) =>
    save.mutate(
      {
        dimension,
        targets: clear ? [] : filled.map((k, i) => ({ key: k, percent: ((parsed[i] ?? 0) / 100).toFixed(2) })),
      },
      { onSuccess: onClose },
    );
  const addCurrency = () => {
    const code = added.trim().toUpperCase();
    if (!/^[A-Z]{3}$/.test(code) || keys.includes(code)) return;
    setKeys([...keys, code]);
    setAdded("");
  };
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-md" data-testid="allocation-dialog">
        <DialogHeader>
          <DialogTitle>{dimension === "kind" ? t("allocation.dialogTitle") : t("allocation.dialogCurrencies")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-2">
          {keys.map((k) => {
            const row = rows.find((r) => r.key === k);
            return (
              <div key={k} className="grid grid-cols-[1fr_5.5rem] items-center gap-3">
                <Label htmlFor={`allocation-${k}`} className="grid gap-0.5 font-normal">
                  <span>{name(k)}</span>
                  <span className="text-xs text-muted-foreground">{t("allocation.now", { share: percent(row?.share ?? "0") })}</span>
                </Label>
                <Input
                  id={`allocation-${k}`}
                  inputMode="decimal"
                  placeholder="—"
                  value={values[k] ?? ""}
                  onChange={(e) => setValues({ ...values, [k]: e.target.value })}
                  aria-invalid={(values[k] ?? "").trim() !== "" && hundredths(values[k]) === null}
                />
              </div>
            );
          })}
          {dimension === "currency" && (
            <div className="flex items-end gap-2">
              <div className="grid flex-1 gap-1">
                <Label htmlFor="allocation-add-currency">{t("allocation.addCurrency")}</Label>
                <Input id="allocation-add-currency" value={added} placeholder="CNY" maxLength={3} onChange={(e) => setAdded(e.target.value)} />
              </div>
              <Button type="button" variant="outline" onClick={addCurrency}>
                {t("allocation.add")}
              </Button>
            </div>
          )}
          <p className={cn("text-sm tabular-nums", filled.length > 0 && sum !== 10_000 && "text-amber-700 dark:text-amber-400")} data-testid="allocation-sum">
            {filled.length > 0 && sum !== 10_000
              ? t("allocation.sumOff", { sum: percent((sum / 100).toFixed(2)) })
              : t("allocation.sum", { sum: percent((sum / 100).toFixed(2)) })}
          </p>
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("allocation.saveFailed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter className="gap-2">
          {rows.some((r) => r.target !== null) && (
            <Button type="button" variant="ghost" className="mr-auto" disabled={save.isPending} onClick={() => submit(true)}>
              {t("allocation.clear")}
            </Button>
          )}
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || filled.length === 0 || save.isPending} onClick={() => submit(false)}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
