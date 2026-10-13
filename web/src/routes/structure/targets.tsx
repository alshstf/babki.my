import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import {
  useAllocation,
  useRebalance,
  useSetAllocationMode,
  useSetTargets,
  useTopUp,
  type Allocation,
  type AllocationRow,
  type AllocationSale,
  type TopUpMoney,
} from "@/api/allocation";
import { useSession } from "@/api/session";
import { formatMinorCompact, minorToInput, parseToMinor } from "@/lib/money";
import { cn } from "@/lib/utils";
import { hundredths, percent } from "@/lib/percent";

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
        {kindTargets && <ModePicker mode={a.mode} canEdit={canEdit} />}
        {kindTargets &&
          (a.mode === "rebalance" ? (
            <Rebalance allocation={a} valuation={valuation} onPreview={setPreview} />
          ) : (
            <TopUp allocation={a} valuation={valuation} onPreview={setPreview} />
          ))}
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

// Moved is what a top-up or a rebalance moves into or out of a kind, the
// same as a share of the worth after it, and the kind's share after it; a
// rebalance also says what of the deposits to take later, what cannot be
// sold now, and what the sale is estimated to gain.
interface Moved {
  change: number;
  part: number;
  shareAfter: number;
  later?: number;
  kept?: number;
  sale?: AllocationSale | null;
  // longYears: a lot held longer is free of the sale's tax; 0 none.
  longYears?: number;
}

// HELD is what a kind holds on a bar, in the page's calm bar colour, green
// being kept for money put in; with a top-up counted the kinds it does not
// touch go grey (STILL), so the ones it moves stand out. SEGMENT is that move: half again as tall as the bar on
// each side, never thinner than a few pixels; ADDED is money put in, a
// see-through green; HATCH fills money taken out.
const HELD = "bg-bar";
const STILL = "bg-muted-foreground";
const SEGMENT = "absolute -top-1 -bottom-1 rounded-sm ring-2 ring-background";
const ADDED = "border border-emerald-600/60 bg-emerald-500/40 dark:border-emerald-400/60 dark:bg-emerald-400/35";
const HATCH = "repeating-linear-gradient(135deg, var(--muted-foreground) 0 2px, transparent 2px 6px)";
const THIN = "6px";

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
        // With a top-up counted the bar is the worth after it: what the
        // kind holds now — thinner, as the worth grows, — what comes in on
        // top of it, what leaves.
        const added = moved && moved.part > 0 ? moved.part : 0;
        const taken = moved && moved.part < 0 ? -moved.part : 0;
        const held = Math.min(Math.max(after - added, 0), 100);
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
              <div
                className={cn("absolute inset-y-0 left-0 rounded", moved && moved.change === 0 ? STILL : HELD)}
                style={{ width: `${held}%` }}
                data-testid={moved && moved.change === 0 ? "allocation-still" : undefined}
              />
              {moved && moved.change > 0 && (
                <div
                  className={cn(SEGMENT, ADDED)}
                  style={{ left: `${held}%`, width: `max(${Math.min(added, 100 - held)}%, ${THIN})` }}
                  data-testid="allocation-added"
                />
              )}
              {moved && moved.change < 0 && (
                <div
                  className={cn(SEGMENT, "border border-muted-foreground/60 bg-background")}
                  style={{ left: `${held}%`, width: `max(${Math.min(taken, 100 - held)}%, ${THIN})`, backgroundImage: HATCH }}
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
              <MovedLine kind={r.key} moved={moved} before={before} target={target} currency={currency} />
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

// MovedLine says what a top-up or a rebalance does to a kind: what goes in
// or out, the share it comes to against the one before, and whether that is
// closer to the target; under it, a rebalance's notes.
function MovedLine({ kind, moved, before, target, currency }: { kind: string; moved: Moved; before: number; target: number | null; currency: string }) {
  const { t } = useTranslation();
  const shares = { after: percent(String(moved.shareAfter)), before: percent(String(before)) };
  const head = moved.change !== 0 ? `${signed(moved.change, currency)} → ` : "→ ";
  const notes = movedNotes(t, kind, moved, currency);
  if (target === null) {
    return (
      <div className={cn(LINE, "text-muted-foreground")}>
        {head + t("allocation.movedNoTarget", shares)}
        <Notes notes={notes} />
      </div>
    );
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
        now < was || now < 0.005
          ? "text-emerald-700 dark:text-emerald-400"
          : now > was
            ? "text-amber-700 dark:text-amber-400"
            : "text-muted-foreground",
      )}
      data-testid="allocation-moved"
    >
      {head + t("allocation.moved", { ...shares, target: percent(String(target)), verdict })}
      <Notes notes={notes} />
    </div>
  );
}

function Notes({ notes }: { notes: string[] }) {
  return notes.map((n) => (
    <div key={n} className="text-muted-foreground" data-testid="allocation-note">
      {n}
    </div>
  ));
}

// movedNotes are a rebalance's words under a kind: what a sale of its papers
// is estimated to gain, what of the deposits to take as they end, what
// cannot be sold now.
function movedNotes(t: (key: string, v?: Record<string, unknown>) => string, key: string, moved: Moved, currency: string): string[] {
  const out: string[] = [];
  const s = moved.sale;
  if (moved.longYears !== undefined && moved.change < 0 && !s && key !== "broker_cash") out.push(t("allocation.saleNotEstimated"));
  if (s && !s.known) out.push(t("allocation.saleUnknown"));
  if (s && s.known) {
    const gain = formatMinorCompact(Math.abs(s.gain_minor), currency);
    out.push(
      s.gain_minor < 0
        ? t("allocation.saleLoss", { amount: gain })
        : s.free_minor > 0
          ? t("allocation.saleGainFree", { amount: gain, free: formatMinorCompact(s.free_minor, currency), years: moved.longYears ?? 0 })
          : t("allocation.saleGain", { amount: gain }),
    );
  }
  if (moved.later) out.push(t("allocation.later", { amount: formatMinorCompact(moved.later, currency) }));
  if (moved.kept) out.push(t("allocation.kept", { amount: formatMinorCompact(moved.kept, currency) }));
  return out;
}

// ModePicker is how the family brings its worth to the targets (Р-37): by
// top-ups only, or by buying and selling; who may not change it reads it.
function ModePicker({ mode, canEdit }: { mode: Allocation["mode"]; canEdit: boolean }) {
  const { t } = useTranslation();
  const set = useSetAllocationMode();
  if (!canEdit) {
    return (
      <p className="text-xs text-muted-foreground">
        {mode === "rebalance" ? t("allocation.modeIsRebalance") : t("allocation.modeIsTopUp")}
      </p>
    );
  }
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <Label htmlFor="allocation-mode" className="font-normal">
        {t("allocation.mode")}
      </Label>
      <Select value={mode} onValueChange={(v) => set.mutate(v === "rebalance" ? "rebalance" : "topup")}>
        <SelectTrigger id="allocation-mode" className="w-60">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="topup">{t("allocation.modes.topup")}</SelectItem>
          <SelectItem value="rebalance">{t("allocation.modes.rebalance")}</SelectItem>
        </SelectContent>
      </Select>
      {set.isError && <span className="text-xs text-red-700 dark:text-red-400">{t("allocation.modeFailed")}</span>}
    </div>
  );
}

// Legend is what the marks on the bars mean while a top-up or a rebalance
// is counted.
function Legend({ items }: { items: { swatch: "held" | "in" | "out" | "still" | "target"; label: string }[] }) {
  return (
    <p className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground" data-testid="allocation-topup-legend">
      {items.map(({ swatch, label }) => (
        <span key={swatch} className="flex items-center gap-1.5">
          {swatch === "held" && <span className={cn("inline-block h-2 w-4 rounded-sm", HELD)} />}
          {swatch === "in" && <span className={cn("inline-block h-3 w-4 rounded-sm", ADDED)} />}
          {swatch === "out" && <span className="inline-block h-3 w-4 rounded-sm border border-muted-foreground/60" style={{ backgroundImage: HATCH }} />}
          {swatch === "still" && <span className={cn("inline-block h-2 w-4 rounded-sm", STILL)} />}
          {swatch === "target" && <span className="inline-block h-3 w-0.5 bg-foreground" />}
          {label}
        </span>
      ))}
    </p>
  );
}

// Rebalance asks what to sell and buy to bring the worth to the targets
// (variant В of Р-37), with new money or none: the kinds above sell down,
// the money buys the kinds below, deposits wait for their end, frozen papers
// stay. What it moves shows on the kinds' bars, the sales' tax under them.
function Rebalance({
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
  const [fresh, setFresh] = useState("");
  const freshMinor = parseToMinor(fresh.trim() === "" ? "0" : fresh);
  const valid = freshMinor !== null && freshMinor >= 0;
  // Asked once the typing stops; with nothing typed, without new money.
  const [asked, setAsked] = useState<number | null>(null);
  useEffect(() => {
    const id = setTimeout(() => setAsked(valid ? (freshMinor ?? 0) : null), 300);
    return () => clearTimeout(id);
  }, [valid, freshMinor]);
  const q = useRebalance(asked, valuation);
  const data = asked !== null ? q.data : undefined;
  useEffect(() => {
    const worth = data?.worth_after_minor ?? 0;
    onPreview(
      data
        ? new Map(
            data.kinds.map((k) => [
              k.key,
              {
                change: k.change_minor,
                part: worth > 0 ? (k.change_minor / worth) * 100 : 0,
                shareAfter: Number(k.share_after),
                later: k.later_minor,
                kept: k.kept_minor,
                sale: k.sale,
                longYears: data.long_holding_years,
              },
            ]),
          )
        : null,
    );
  }, [data, onPreview]);
  useEffect(() => () => onPreview(null), [onPreview]);
  const counted = data && !q.isPlaceholderData ? data : null;
  return (
    <div className="grid gap-2 rounded-md border bg-muted/30 p-3" data-testid="allocation-rebalance">
      <p className="text-sm font-medium">{t("allocation.rebalanceTitle")}</p>
      <p className="text-xs text-muted-foreground">{t("allocation.rebalanceHint")}</p>
      <div className="grid gap-1">
        <Label htmlFor="allocation-rebalance-fresh">{t("allocation.rebalanceAmount", { currency: c })}</Label>
        <Input
          id="allocation-rebalance-fresh"
          inputMode="decimal"
          className="w-40"
          value={fresh}
          onChange={(e) => setFresh(e.target.value)}
          aria-invalid={!valid}
        />
      </div>
      {counted && (
        <Legend
          items={[
            { swatch: "held", label: t("allocation.legendHeld") },
            { swatch: "in", label: t("allocation.legendBuy") },
            { swatch: "out", label: t("allocation.legendSell") },
            { swatch: "still", label: t("allocation.legendUntouched") },
            { swatch: "target", label: t("allocation.legendTarget") },
          ]}
        />
      )}
      {counted && (
        <p className="text-sm" data-testid="allocation-rebalance-total">
          {counted.bought_minor === 0
            ? t("allocation.rebalanceNothing")
            : t("allocation.rebalanceTotal", {
                sold: formatMinorCompact(counted.sold_minor, c),
                bought: formatMinorCompact(counted.bought_minor, c),
              })}
          {counted.from_brokers_minor > 0 && ` ${t("allocation.rebalanceFromBrokers", { amount: formatMinorCompact(counted.from_brokers_minor, c) })}`}
          {counted.amount_minor > 0 && ` ${t("allocation.rebalanceFresh", { amount: formatMinorCompact(counted.amount_minor, c) })}`}
        </p>
      )}
      {counted && counted.sold_minor > 0 && <TaxLine r={counted} currency={c} />}
      {q.isError && <p className="text-xs text-red-700 dark:text-red-400">{t("allocation.topUpFailed")}</p>}
    </div>
  );
}

// TaxLine is the estimate of the tax a rebalance's sales would bring, and
// what it leaves out — or why there is none.
function TaxLine({ r, currency }: { r: { tax_minor: number | null; tax_rate: string | null; long_holding_years: number; kinds: { sale: AllocationSale | null }[] }; currency: string }) {
  const { t } = useTranslation();
  if (!r.kinds.some((k) => k.sale !== null)) return null;
  let text: string;
  if (r.tax_rate === null) text = t("allocation.taxNoRules");
  else if (r.tax_minor === null) text = t("allocation.taxUnknown");
  else {
    const rate = percent(r.tax_rate);
    text =
      r.long_holding_years > 0
        ? t("allocation.taxLong", { tax: formatMinorCompact(r.tax_minor, currency), rate, years: r.long_holding_years })
        : t("allocation.tax", { tax: formatMinorCompact(r.tax_minor, currency), rate });
  }
  return (
    <p className="text-xs text-muted-foreground" data-testid="allocation-tax">
      {text}
    </p>
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
    const worth = data?.worth_after_minor ?? 0;
    onPreview(
      data
        ? new Map(
            data.kinds.map((k) => [k.key, { change: k.change_minor, part: worth > 0 ? (k.change_minor / worth) * 100 : 0, shareAfter: Number(k.share_after) }]),
          )
        : null,
    );
  }, [data, onPreview]);
  useEffect(() => () => onPreview(null), [onPreview]);
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
        <Legend
          items={[
            { swatch: "held", label: t("allocation.legendHeld") },
            { swatch: "in", label: t("allocation.legendIn") },
            ...(counted.fromBrokers > 0 ? [{ swatch: "out" as const, label: t("allocation.legendOut") }] : []),
            { swatch: "still", label: t("allocation.legendStill") },
            { swatch: "target", label: t("allocation.legendTarget") },
          ]}
        />
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
