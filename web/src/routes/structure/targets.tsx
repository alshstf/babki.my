import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useAllocation, useSetTargets, useTopUp, type Allocation, type AllocationRow, type TopUpMoney } from "@/api/allocation";
import { useSession } from "@/api/session";
import { formatMinorCompact, minorToInput, parseToMinor } from "@/lib/money";
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
// its target; a top-up asked about shows on the kinds' bars what it moves.
export function TargetsCard({ valuation }: { valuation: "liquid" | "full" }) {
  const { t } = useTranslation();
  const allocation = useAllocation(valuation);
  const { data: session } = useSession();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const [editing, setEditing] = useState<"kind" | "currency" | null>(null);
  const [preview, setPreview] = useState<Map<string, Moved> | null>(null);
  const a = allocation.data;
  if (!a || (a.worth_minor === 0 && !canEdit)) return null;
  const c = a.base_currency;
  const kindTargets = a.kinds.some((r) => r.target !== null);
  const currencyTargets = a.currencies.some((r) => r.target !== null);
  return (
    <Card className="min-w-0" data-testid="allocation">
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{t("allocation.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">
          {valuation === "full"
            ? t("allocation.hintFull", { worth: formatMinorCompact(a.worth_minor, c) })
            : t("allocation.hint", { worth: formatMinorCompact(a.worth_minor, c) })}
        </p>
      </CardHeader>
      <CardContent className="grid gap-4">
        {!kindTargets && <p className="text-sm text-muted-foreground">{t("allocation.empty")}</p>}
        {kindTargets && <TopUp allocation={a} valuation={valuation} onPreview={setPreview} />}
        <Rows rows={a.kinds} currency={c} name={(k) => kindName(t, k, c)} testId="allocation-kinds" preview={kindTargets ? preview : null} />
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
          <Rows rows={a.currencies} currency={c} name={(k) => currencyName(t, k)} testId="allocation-currencies" preview={null} />
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

// Moved is what a top-up moves into a kind (or out of the brokers' free
// money) and the kind's share after it.
interface Moved {
  change: number;
  shareAfter: number;
}

// SEGMENT is what a top-up moves on a bar: half again as tall as the bar on
// each side; HATCH fills what leaves.
const SEGMENT = "absolute -top-1 -bottom-1 rounded-sm ring-2 ring-background";
const HATCH = "repeating-linear-gradient(135deg, rgb(217 119 6 / 0.75) 0 3px, transparent 3px 6px)";

// LINE places the words under a row's bar: across the row on a phone, under
// the bar on a wide screen.
const LINE = "col-span-2 row-start-3 text-xs md:col-span-2 md:col-start-2 md:row-start-2";

// The sign written out: «+48 036 ₽», «−1 722 433 ₽».
function signed(minor: number, currency: string): string {
  return minor > 0 ? `+${formatMinorCompact(minor, currency)}` : `−${formatMinorCompact(-minor, currency)}`;
}

function Rows({
  rows,
  currency,
  name,
  testId,
  preview,
}: {
  rows: AllocationRow[];
  currency: string;
  name: (key: string) => string;
  testId: string;
  preview: Map<string, Moved> | null;
}) {
  const { t } = useTranslation();
  return (
    <div className="grid min-w-0 gap-3" data-testid={testId}>
      {rows.map((r) => {
        const gap = r.gap === null ? null : Number(r.gap);
        const notable = gap !== null && Math.abs(gap) > NOTABLE_GAP;
        const before = Number(r.share);
        const moved = preview?.get(r.key);
        const after = moved?.shareAfter ?? before;
        const target = r.target === null ? null : Number(r.target);
        return (
          // On a wide screen the name, the bar and the figures sit in one
          // line, the words under the bar; on a phone one under another.
          <div
            key={r.key}
            className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 gap-y-1 md:grid-cols-[12rem_minmax(0,1fr)_auto]"
            data-testid="allocation-row"
          >
            <span className="col-start-1 row-start-1 min-w-0 text-sm">{name(r.key)}</span>
            <div className="relative col-span-2 row-start-2 h-2 rounded bg-muted md:col-span-1 md:col-start-2 md:row-start-1">
              <div className="absolute inset-y-0 left-0 rounded bg-emerald-500" style={{ width: `${Math.min(before, after, 100)}%` }} />
              {/* What a top-up moves stands out above and below the bar and is
                  never thinner than a few pixels, however small: money put
                  in — blue; money taken out (the brokers' free money) —
                  hatched. A share that only thins as the worth grows gets
                  its words, not a mark. */}
              {moved && moved.change > 0 && (
                <div
                  className={SEGMENT + " bg-sky-500 dark:bg-sky-400"}
                  style={{ left: `${Math.min(before, 100)}%`, width: `max(${Math.min(after, 100) - Math.min(before, 100)}%, 6px)` }}
                  data-testid="allocation-added"
                />
              )}
              {moved && moved.change < 0 && (
                <div
                  className={SEGMENT + " border border-amber-600 bg-background"}
                  style={{ left: `${Math.min(after, 100)}%`, width: `max(${Math.min(before, 100) - Math.min(after, 100)}%, 6px)`, backgroundImage: HATCH }}
                  data-testid="allocation-removed"
                />
              )}
              {target !== null && (
                <div
                  className="absolute -top-1.5 z-10 h-5 w-0.5 bg-foreground"
                  style={{ left: `calc(${Math.min(target, 100)}% - 1px)` }}
                  title={t("allocation.targetTick", { target: percent(r.target ?? "0") })}
                />
              )}
            </div>
            <span className="col-start-2 row-start-1 text-right text-sm tabular-nums md:col-start-3">
              {formatMinorCompact(r.minor, currency)} <span className="text-muted-foreground">{percent(r.share)}</span>
            </span>
            {moved ? (
              <MovedLine moved={moved} before={before} target={target} currency={currency} />
            ) : (
              r.target !== null &&
              gap !== null &&
              r.off_minor !== null && (
                <div className={cn(LINE, notable ? "text-amber-700 dark:text-amber-400" : "text-muted-foreground")}>
                  {gap === 0
                    ? t("allocation.onTarget", { target: percent(r.target) })
                    : gap > 0
                      ? t("allocation.above", { target: percent(r.target), gap: percent(String(gap)).replace(" %", ""), amount: formatMinorCompact(r.off_minor, currency) })
                      : t("allocation.below", { target: percent(r.target), gap: percent(String(-gap)).replace(" %", ""), amount: formatMinorCompact(-r.off_minor, currency) })}
                </div>
              )
            )}
          </div>
        );
      })}
    </div>
  );
}

// MovedLine says what a top-up does to a kind: what goes in or out, the
// share it comes to against the one before, and whether that is closer to
// the target.
function MovedLine({ moved, before, target, currency }: { moved: Moved; before: number; target: number | null; currency: string }) {
  const { t } = useTranslation();
  const shares = { after: percent(String(moved.shareAfter)), before: percent(String(before)) };
  const head = moved.change !== 0 ? `${signed(moved.change, currency)} → ` : "→ ";
  if (target === null) {
    return <div className={cn(LINE, "text-muted-foreground")}>{head + t("allocation.movedNoTarget", shares)}</div>;
  }
  const was = Math.abs(before - target);
  const now = Math.abs(moved.shareAfter - target);
  const by = percent(Math.abs(was - now).toFixed(2)).replace(" %", "");
  const verdict =
    now < 0.005
      ? t("allocation.verdictOn")
      : now < was
        ? t("allocation.verdictCloser", { by })
        : now > was
          ? t("allocation.verdictFurther", { by })
          : t("allocation.verdictSame");
  return (
    <div
      className={cn(
        LINE,
        now <= was ? "text-emerald-700 dark:text-emerald-400" : "text-amber-700 dark:text-amber-400",
      )}
      data-testid="allocation-moved"
    >
      {head + t("allocation.moved", { ...shares, target: percent(String(target)), verdict })}
    </div>
  );
}

// TopUp asks where money goes by the targets (variant Б of Р-37): new money,
// and of the brokers' free money what to put in too — it leaves the brokers'
// line and goes into the kinds, the worth unchanged by it. What it moves
// shows on the kinds' bars.
function TopUp({
  allocation,
  valuation,
  onPreview,
}: {
  allocation: Allocation;
  valuation: "liquid" | "full";
  onPreview: (moved: Map<string, Moved> | null) => void;
}) {
  const { t } = useTranslation();
  const c = allocation.base_currency;
  const free = allocation.kinds.find((r) => r.key === "broker_cash")?.minor ?? 0;
  const [fresh, setFresh] = useState("");
  const [useFree, setUseFree] = useState(false);
  const [fromBrokers, setFromBrokers] = useState(minorToInput(free));
  const freshMinor = parseToMinor(fresh.trim() === "" ? "0" : fresh);
  const brokersMinor = useFree ? parseToMinor(fromBrokers.trim() === "" ? "0" : fromBrokers) : 0;
  const brokersOk = brokersMinor !== null && brokersMinor >= 0 && brokersMinor <= free;
  const valid = freshMinor !== null && freshMinor >= 0 && brokersOk && (freshMinor ?? 0) + (brokersMinor ?? 0) > 0;
  // Asked once the typing stops, not at every key.
  const [asked, setAsked] = useState<TopUpMoney | null>(null);
  useEffect(() => {
    const money = valid ? { fresh: freshMinor ?? 0, fromBrokers: brokersMinor ?? 0 } : null;
    const id = setTimeout(() => setAsked(money), 300);
    return () => clearTimeout(id);
  }, [valid, freshMinor, brokersMinor]);
  const topUp = useTopUp(asked, valuation);
  const data = asked ? topUp.data : undefined;
  useEffect(() => {
    onPreview(data ? new Map(data.kinds.map((k) => [k.key, { change: k.change_minor, shareAfter: Number(k.share_after) }])) : null);
  }, [data, onPreview]);
  // The words say what was counted, not what is being typed.
  const counted = data && asked && !topUp.isPlaceholderData ? asked : null;
  return (
    <div className="grid gap-2 rounded-md border bg-muted/30 p-3" data-testid="allocation-topup">
      <p className="text-sm font-medium">{t("allocation.topUpTitle")}</p>
      <p className="text-xs text-muted-foreground">{t("allocation.topUpHint")}</p>
      <div className="flex flex-wrap items-end gap-x-4 gap-y-2">
        <div className="grid gap-1">
          <Label htmlFor="allocation-topup">{t("allocation.topUpAmount", { currency: c })}</Label>
          <Input id="allocation-topup" inputMode="decimal" className="w-40" value={fresh} onChange={(e) => setFresh(e.target.value)} />
        </div>
        {free > 0 && (
          <div className="grid gap-1">
            <Label htmlFor="allocation-use-free" className="flex items-center gap-2 font-normal">
              <Checkbox id="allocation-use-free" checked={useFree} onCheckedChange={(v) => setUseFree(v === true)} />
              {t("allocation.topUpUseFree", { amount: formatMinorCompact(free, c) })}
            </Label>
            {useFree && (
              <Input
                aria-label={t("allocation.topUpFromBrokers", { currency: c })}
                inputMode="decimal"
                className="w-40"
                value={fromBrokers}
                onChange={(e) => setFromBrokers(e.target.value)}
                aria-invalid={!brokersOk}
              />
            )}
          </div>
        )}
      </div>
      {useFree && !brokersOk && <p className="text-xs text-red-700 dark:text-red-400">{t("allocation.topUpTooMuch", { amount: formatMinorCompact(free, c) })}</p>}
      {counted && (
        <p className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground" data-testid="allocation-topup-legend">
          <span className="flex items-center gap-1.5">
            <span className="inline-block h-3 w-4 rounded-sm bg-sky-500 dark:bg-sky-400" />
            {t("allocation.legendIn")}
          </span>
          {counted.fromBrokers > 0 && (
            <span className="flex items-center gap-1.5">
              <span className="inline-block h-3 w-4 rounded-sm border border-amber-600" style={{ backgroundImage: HATCH }} />
              {t("allocation.legendOut")}
            </span>
          )}
          <span className="flex items-center gap-1.5">
            <span className="inline-block h-3 w-0.5 bg-foreground" />
            {t("allocation.legendTarget")}
          </span>
        </p>
      )}
      {counted && (
        <p className="text-sm" data-testid="allocation-topup-total">
          {counted.fromBrokers > 0
            ? t("allocation.topUpTotalWithBrokers", {
                fresh: formatMinorCompact(counted.fresh, c),
                brokers: formatMinorCompact(counted.fromBrokers, c),
                total: formatMinorCompact(counted.fresh + counted.fromBrokers, c),
              })
            : t("allocation.topUpTotal", { total: formatMinorCompact(counted.fresh, c) })}
        </p>
      )}
      {topUp.isError && <p className="text-xs text-red-700 dark:text-red-400">{t("allocation.topUpFailed")}</p>}
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
