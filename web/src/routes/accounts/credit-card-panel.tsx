import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import type { AccountWithBalance } from "@/api/accounts";
import { useCreditCard, useSetCreditCard, type CreditCard, type CreditCardTerms } from "@/api/credit-cards";
import { formatMinor, minorToInput, parseToMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { SOON_DAYS, daysUntil } from "@/lib/card-due";
import { cn } from "@/lib/utils";
import { PushToggle } from "@/components/push-toggle";
import { useCategories } from "@/api/categories";
import { CategorySelect, categoryLabel } from "@/components/category-picker";

const pct = (s: string) => Number(s).toLocaleString("ru-RU");

// CreditCardPanel is a credit card's terms and what is due on it (decision
// Р-26): the debt and what is left of the limit, what to pay by when so that
// purchases stay free of interest, the minimum payment, and the purchases
// whose grace has run out.
export function CreditCardPanel({ account, canEdit }: { account: AccountWithBalance; canEdit: boolean }) {
  const { t } = useTranslation();
  const card = useCreditCard(account.id, true);
  const [editing, setEditing] = useState(false);
  if (card.isPending) return null;
  const data = card.data;
  if (!data) {
    return (
      <div className="flex flex-wrap items-center gap-3 rounded-lg border border-dashed p-4 text-sm" data-testid="card-panel">
        <span className="text-muted-foreground">{t("card.noTerms")}</span>
        {canEdit && (
          <Button size="sm" variant="outline" onClick={() => setEditing(true)}>
            {t("card.setTerms")}
          </Button>
        )}
        {editing && <TermsDialog account={account} onClose={() => setEditing(false)} />}
      </div>
    );
  }
  const c = account.currency;
  const st = data.status;
  const next = st.grace[0];
  return (
    <div className="grid gap-3 rounded-lg border p-4" data-testid="card-panel">
      <div className="grid gap-3 sm:grid-cols-3">
        <div>
          <div className="text-sm text-muted-foreground">{st.debt_minor >= 0 ? t("card.debt") : t("card.ownMoney")}</div>
          <div className="text-xl font-semibold tabular-nums" data-testid="card-debt">
            {formatMinor(Math.abs(st.debt_minor), c)}
          </div>
        </div>
        <div>
          <div className="text-sm text-muted-foreground">{t("card.available")}</div>
          <div className="text-xl font-semibold tabular-nums" data-testid="card-available">
            {formatMinor(st.available_minor, c)}
          </div>
          <div className="text-xs text-muted-foreground">{t("card.ofLimit", { limit: formatMinor(data.terms.limit_minor, c) })}</div>
        </div>
        <div>
          <div className="text-sm text-muted-foreground">{st.minimum_estimate ? t("card.minimumEstimate") : t("card.minimum")}</div>
          <div
            className={cn("text-xl font-semibold tabular-nums", st.minimum_missed && "text-red-700 dark:text-red-400")}
            data-testid="card-minimum"
          >
            {st.minimum_minor > 0 ? formatMinor(st.minimum_minor, c) : t("card.minimumPaid")}
          </div>
          {st.minimum_minor > 0 && (
            <div className={cn("text-xs", st.minimum_missed ? "text-red-700 dark:text-red-400" : "text-muted-foreground")}>
              {st.minimum_missed ? t("card.minimumMissed", { date: formatDate(st.minimum_on) }) : t("card.by", { date: formatDate(st.minimum_on) })}
            </div>
          )}
          {st.penalty_minor > 0 && (
            <div className="text-xs text-red-700 dark:text-red-400" data-testid="card-penalty">
              {t("card.penalty", { amount: formatMinor(st.penalty_minor, c) })}
            </div>
          )}
        </div>
      </div>

      {data.by_journal ? (
        <div className="grid gap-1 text-sm" data-testid="card-grace">
          {next ? (
            <div className={cn(daysUntil(next.on) <= SOON_DAYS && "font-medium text-amber-700 dark:text-amber-400")}>
              {t("card.graceNext", { amount: formatMinor(next.amount_minor, c), date: formatDate(next.on) })}
            </div>
          ) : (
            st.lost.length === 0 && <div className="text-muted-foreground">{t("card.graceClear")}</div>
          )}
          {st.grace.slice(1).map((g) => (
            <div key={g.on} className="text-muted-foreground">
              {t("card.graceLater", { amount: formatMinor(g.amount_minor, c), date: formatDate(g.on) })}
            </div>
          ))}
        </div>
      ) : (
        <p className="text-sm text-muted-foreground" data-testid="card-by-balance">
          {t("card.byBalance")}
        </p>
      )}

      {st.grace_off_since && (
        <Alert variant="destructive" data-testid="card-grace-off">
          <AlertDescription>
            {(st.grace_off_by_minimum ? t("card.graceOffByMinimum", { date: formatDate(st.grace_off_since) }) : t("card.graceOff", { date: formatDate(st.grace_off_since) })) +
              " " +
              t("card.graceOffRestore", { amount: formatMinor(st.to_restore_minor, c) })}
          </AlertDescription>
        </Alert>
      )}
      {st.lost.map((l) => {
        const values = {
          from: formatDate(l.from),
          to: formatDate(l.to),
          amount: formatMinor(l.amount_minor, c),
          interest: formatMinor(l.interest_minor, c),
        };
        return (
          <Alert key={l.from} variant="destructive" data-testid="card-lost">
            <AlertDescription>{l.early ? t("card.lostEarly", values) : t("card.lost", values)}</AlertDescription>
          </Alert>
        );
      })}
      {st.non_grace_minor > 0 && (
        <p className="text-sm text-amber-700 dark:text-amber-400" data-testid="card-non-grace">
          {t("card.nonGrace", { amount: formatMinor(st.non_grace_minor, c), interest: formatMinor(st.non_grace_interest_minor, c) })}
        </p>
      )}

      {st.cashback_on && (
        <p className="text-sm text-muted-foreground" data-testid="card-cashback">
          {data.terms.cashback.points
            ? t("card.cashbackPoints", { amount: (st.cashback_expected_minor / 100).toLocaleString("ru-RU"), date: formatDate(st.cashback_on) })
            : t("card.cashbackExpected", { amount: formatMinor(st.cashback_expected_minor, c), date: formatDate(st.cashback_on) })}
        </p>
      )}
      {data.terms.fees.cash_free_minor > 0 && (
        <p className="text-sm text-muted-foreground" data-testid="card-cash">
          {t("card.cashThisPeriod", {
            taken: formatMinor(st.cash_this_period_minor, c),
            free: formatMinor(data.terms.fees.cash_free_minor, c),
          })}
        </p>
      )}

      {data.benefit && <BenefitBlock benefit={data.benefit} currency={c} ownRate={data.terms.own_rate} />}

      <div className="text-xs text-muted-foreground">
        {t("card.termsLine", {
          day: data.terms.statement_day,
          pay: data.terms.pay_by_period_end ? t("card.payByPeriodEndShort") : t("card.payDays", { days: data.terms.payment_days }),
          grace: graceLine(t, data.terms),
          min: pct(data.terms.min_percent),
          floor: formatMinor(data.terms.min_floor_minor, c),
          charges: data.terms.charges_in_full ? t("card.chargesInFullShort") : "",
          rate: pct(data.terms.annual_rate),
        })}
        {data.terms.grace_all_lost && " " + t("card.graceAllLostShort")}
        {feesLine(t, data.terms.fees, c) && " " + feesLine(t, data.terms.fees, c)}
      </div>
      <p className="text-xs text-muted-foreground">{t("card.hint")}</p>
      <PushToggle />
      {canEdit && (
        <Button size="sm" variant="outline" className="justify-self-start" onClick={() => setEditing(true)}>
          {t("card.editTerms")}
        </Button>
      )}
      {editing && <TermsDialog account={account} card={data} onClose={() => setEditing(false)} />}
    </div>
  );
}

// feesLine is the tariff's fees for the line of the card's terms; empty
// when it has none.
function feesLine(t: (key: string, values?: Record<string, unknown>) => string, fees: CreditCardTerms["fees"], c: string): string {
  const parts: string[] = [];
  if (fees.monthly_minor > 0) parts.push(t("card.feeMonthly", { amount: formatMinor(fees.monthly_minor, c) }));
  if (Number(fees.cash_percent) > 0 || fees.cash_fixed_minor > 0) {
    parts.push(t("card.feeCash", { free: formatMinor(fees.cash_free_minor, c), pct: pct(fees.cash_percent), fixed: formatMinor(fees.cash_fixed_minor, c) }));
  }
  if (Number(fees.transfer_percent) > 0 || fees.transfer_fixed_minor > 0) {
    parts.push(t("card.feeTransfer", { pct: pct(fees.transfer_percent), fixed: formatMinor(fees.transfer_fixed_minor, c) }));
  }
  if (Number(fees.penalty_daily_percent) > 0) parts.push(t("card.feePenalty", { pct: pct(fees.penalty_daily_percent) }));
  return parts.length ? t("card.feesLine", { list: parts.join("; ") }) : "";
}

// graceLine says how the card's grace runs, for the line of its terms.
function graceLine(t: (key: string, values?: Record<string, unknown>) => string, terms: CreditCardTerms): string {
  switch (terms.grace_kind) {
    case "long":
      return t("card.graceLong", { days: terms.grace_days });
    case "windows":
      return t("card.graceWindows", { window: terms.window_months, months: terms.grace_months });
    case "running":
      return t("card.graceRunning", { days: terms.grace_days });
    default:
      return t("card.graceStatement");
  }
}

// BenefitBlock weighs the card against the family's own money over the last
// year (decision Р-26): what the own money earned meanwhile, the cashback, the
// bank's charges.
function BenefitBlock({ benefit, currency, ownRate }: { benefit: NonNullable<CreditCard["benefit"]>; currency: string; ownRate: string | null }) {
  const { t } = useTranslation();
  const b = benefit;
  return (
    <div className="grid gap-1 rounded-md bg-muted/40 p-3 text-sm" data-testid="card-benefit">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="font-medium">{t("card.benefitTitle", { from: formatDate(b.from) })}</span>
        <span
          className={cn("text-lg font-semibold tabular-nums", b.total_minor >= 0 ? "text-emerald-700 dark:text-emerald-400" : "text-red-700 dark:text-red-400")}
          data-testid="card-benefit-total"
        >
          {b.total_minor > 0 ? "+" : ""}
          {formatMinor(b.total_minor, currency)}
        </span>
      </div>
      {b.own_rate_known ? (
        <div className="text-muted-foreground">
          {t("card.benefitOwn", { amount: formatMinor(b.own_earned_minor, currency), rate: pct(ownRate ?? "0") })}
        </div>
      ) : (
        <div className="text-muted-foreground">{t("card.benefitNoRate")}</div>
      )}
      <div className="text-muted-foreground">{t("card.benefitCashback", { amount: formatMinor(b.cashback_minor, currency) })}</div>
      <div className="text-muted-foreground">
        {b.costs_minor > 0 ? t("card.benefitCosts", { amount: formatMinor(b.costs_minor, currency) }) : t("card.benefitNoCosts")}
      </div>
      {b.pending_interest_minor > 0 && (
        <div className="text-red-700 dark:text-red-400" data-testid="card-benefit-pending">
          {t("card.benefitPending", { amount: formatMinor(b.pending_interest_minor, currency) })}
        </div>
      )}
    </div>
  );
}

// Typical terms to start from; the bank's statement has the real ones.
const LENIENT = { graceAllLost: false, payByPeriodEnd: false, chargesInFull: false };
const PRESETS: Record<string, Partial<Form>> = {
  statement55: { kind: "statement", paymentDays: "25", minPercent: "8", ...LENIENT },
  sber120: { kind: "long", graceDays: "120", paymentDays: "20", minPercent: "3", ...LENIENT, chargesInFull: true },
  year: { kind: "long", graceDays: "365", paymentDays: "20", minPercent: "3", ...LENIENT },
  // ВТБ «Карта возможностей» (#457): one grace of 110 days from the 1st of
  // the first purchase's month, renewed once the debt is repaid; the minimum
  // by the 20th.
  vtb110: {
    kind: "running", graceDays: "110", runFrom: "month_start", statementDay: "1", paymentDays: "19", minPercent: "3",
    minFloor: "0", ...LENIENT,
  },
  // Газпромбанк «180 дней» (decision Р-28): two months of purchases, paid by
  // the end of the sixth; the minimum by the end of the next month.
  gpb180: {
    kind: "windows", windowMonths: "2", graceMonths: "6", statementDay: "1", minPercent: "3", minFloor: "500",
    rate: "59.99", graceAllLost: true, payByPeriodEnd: true, chargesInFull: true,
    cashFree: "100000", cashPercent: "5.9", cashFixed: "590", transferPercent: "4.9", transferFixed: "390", penaltyDaily: "0.1",
  },
};

interface Form {
  limit: string;
  statementDay: string;
  paymentDays: string;
  kind: CreditCardTerms["grace_kind"];
  graceDays: string;
  windowMonths: string;
  graceMonths: string;
  openedOn: string;
  runFrom: CreditCardTerms["grace_run_from"];
  minPercent: string;
  minFloor: string;
  rate: string;
  ownRate: string;
  graceAllLost: boolean;
  payByPeriodEnd: boolean;
  chargesInFull: boolean;
  transferCategories: string[];
  monthlyFee: string;
  cashFree: string;
  cashPercent: string;
  cashFixed: string;
  transferPercent: string;
  transferFixed: string;
  penaltyDaily: string;
  cashbackBase: string;
  cashbackCap: string;
  cashbackDays: string;
  cashbackPoints: boolean;
  cashbackCategories: { id: string; percent: string }[];
}

function TermsDialog({ account, card, onClose }: { account: AccountWithBalance; card?: CreditCard; onClose: () => void }) {
  const { t } = useTranslation();
  const save = useSetCreditCard(account.id);
  const terms = card?.terms;
  const [f, setF] = useState<Form>({
    limit: terms ? minorToInput(terms.limit_minor) : "",
    statementDay: terms ? String(terms.statement_day) : "1",
    paymentDays: terms ? String(terms.payment_days) : "25",
    kind: terms?.grace_kind ?? "statement",
    graceDays: terms && (terms.grace_kind === "long" || terms.grace_kind === "running") ? String(terms.grace_days) : "120",
    runFrom: terms?.grace_run_from ?? "purchase",
    windowMonths: terms && terms.grace_kind === "windows" ? String(terms.window_months) : "2",
    graceMonths: terms && terms.grace_kind === "windows" ? String(terms.grace_months) : "6",
    openedOn: terms?.opened_on ?? "",
    minPercent: terms?.min_percent ?? "3",
    minFloor: terms ? minorToInput(terms.min_floor_minor) : "300",
    rate: terms?.annual_rate ?? "",
    ownRate: terms?.own_rate ?? "",
    graceAllLost: terms?.grace_all_lost ?? false,
    payByPeriodEnd: terms?.pay_by_period_end ?? false,
    chargesInFull: terms?.charges_in_full ?? false,
    transferCategories: terms?.transfer_categories ?? [],
    monthlyFee: terms ? minorToInput(terms.fees.monthly_minor) : "0",
    cashFree: terms ? minorToInput(terms.fees.cash_free_minor) : "0",
    cashPercent: terms?.fees.cash_percent ?? "0",
    cashFixed: terms ? minorToInput(terms.fees.cash_fixed_minor) : "0",
    transferPercent: terms?.fees.transfer_percent ?? "0",
    transferFixed: terms ? minorToInput(terms.fees.transfer_fixed_minor) : "0",
    penaltyDaily: terms?.fees.penalty_daily_percent ?? "0",
    cashbackBase: terms?.cashback.base_percent ?? "0",
    cashbackCap: terms ? minorToInput(terms.cashback.monthly_cap_minor) : "0",
    cashbackDays: terms ? String(terms.cashback.credit_days) : "0",
    cashbackPoints: terms?.cashback.points ?? false,
    cashbackCategories: (terms?.cashback.categories ?? []).map((c) => ({ id: c.category_id, percent: c.percent })),
  });
  const categories = useCategories();
  const set = (patch: Partial<Form>) => setF((prev) => ({ ...prev, ...patch }));
  const num = (s: string) => Number(s.replace(",", "."));
  const int = (s: string, lo: number, hi: number) => /^\d+$/.test(s) && Number(s) >= lo && Number(s) <= hi;
  const limit = parseToMinor(f.limit);
  const floor = parseToMinor(f.minFloor || "0");
  const decimalOk = (s: string, hi: number) => s.trim() !== "" && !Number.isNaN(num(s)) && num(s) >= 0 && num(s) < hi;
  const money = (s: string) => parseToMinor(s.trim() === "" ? "0" : s);
  const fees = {
    monthly_minor: money(f.monthlyFee),
    cash_free_minor: money(f.cashFree),
    cash_fixed_minor: money(f.cashFixed),
    transfer_fixed_minor: money(f.transferFixed),
  };
  const feesValid =
    Object.values(fees).every((v) => v !== null && v >= 0) &&
    decimalOk(f.cashPercent || "0", 100) && decimalOk(f.transferPercent || "0", 100) && decimalOk(f.penaltyDaily || "0", 10);
  const cashbackCap = money(f.cashbackCap);
  const cashbackValid =
    decimalOk(f.cashbackBase || "0", 100) && cashbackCap !== null && cashbackCap >= 0 && int(f.cashbackDays || "0", 0, 60) &&
    f.cashbackCategories.every((c) => decimalOk(c.percent, 100));
  const valid =
    limit !== null && limit >= 0 && floor !== null && floor >= 0 &&
    int(f.statementDay, 1, 31) && (f.payByPeriodEnd || int(f.paymentDays, 0, 60)) &&
    ((f.kind !== "long" && f.kind !== "running") || int(f.graceDays, 1, 1100)) &&
    (f.kind !== "windows" ||
      (int(f.windowMonths, 1, 12) && int(f.graceMonths, Number(f.windowMonths), 36) && /^\d{4}-\d{2}-\d{2}$/.test(f.openedOn))) &&
    decimalOk(f.minPercent, 100.0001) && decimalOk(f.rate, 1000) && (f.ownRate.trim() === "" || decimalOk(f.ownRate, 1000)) &&
    feesValid && cashbackValid;

  const submit = () => {
    if (!valid || limit === null || floor === null) return;
    save.mutate(
      {
        limit_minor: limit,
        statement_day: Number(f.statementDay),
        payment_days: int(f.paymentDays, 0, 60) ? Number(f.paymentDays) : 0,
        grace_kind: f.kind,
        grace_days: f.kind === "long" || f.kind === "running" ? Number(f.graceDays) : 0,
        grace_run_from: f.kind === "running" ? f.runFrom : "purchase",
        window_months: f.kind === "windows" ? Number(f.windowMonths) : 0,
        grace_months: f.kind === "windows" ? Number(f.graceMonths) : 0,
        opened_on: f.kind === "windows" ? f.openedOn : null,
        grace_all_lost: f.graceAllLost,
        pay_by_period_end: f.payByPeriodEnd,
        charges_in_full: f.chargesInFull,
        transfer_categories: f.transferCategories,
        fees: {
          monthly_minor: fees.monthly_minor ?? 0,
          cash_free_minor: fees.cash_free_minor ?? 0,
          cash_percent: String(num(f.cashPercent || "0")),
          cash_fixed_minor: fees.cash_fixed_minor ?? 0,
          transfer_percent: String(num(f.transferPercent || "0")),
          transfer_fixed_minor: fees.transfer_fixed_minor ?? 0,
          penalty_daily_percent: String(num(f.penaltyDaily || "0")),
        },
        cashback: {
          base_percent: String(num(f.cashbackBase || "0")),
          categories: f.cashbackCategories.map((c) => ({ category_id: c.id, percent: String(num(c.percent)) })),
          monthly_cap_minor: cashbackCap ?? 0,
          points: f.cashbackPoints,
          credit_days: Number(f.cashbackDays || "0"),
        },
        min_percent: String(num(f.minPercent)),
        min_floor_minor: floor,
        annual_rate: String(num(f.rate)),
        own_rate: f.ownRate.trim() === "" ? null : String(num(f.ownRate)),
      },
      { onSuccess: onClose },
    );
  };
  type TextField = { [K in keyof Form]: Form[K] extends string ? K : never }[keyof Form];
  const field = (id: TextField, label: string, hint?: string, type = "text") => (
    <div className="grid gap-1">
      <Label htmlFor={`card-${id}`}>{label}</Label>
      <Input
        id={`card-${id}`}
        type={type}
        inputMode={type === "text" ? "decimal" : undefined}
        value={f[id]}
        onChange={(e) => set({ [id]: e.target.value } as Partial<Form>)}
      />
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
  type Switch = "graceAllLost" | "payByPeriodEnd" | "chargesInFull" | "cashbackPoints";
  const toggle = (id: Switch, label: string, hint: string) => (
    <div className="flex items-start gap-2">
      <Checkbox id={`card-${id}`} checked={f[id]} onCheckedChange={(v) => set({ [id]: v === true } as Partial<Form>)} className="mt-0.5" />
      <div className="grid gap-0.5">
        <Label htmlFor={`card-${id}`} className="font-normal">
          {label}
        </Label>
        <p className="text-xs text-muted-foreground">{hint}</p>
      </div>
    </div>
  );
  const presetName: Record<string, string> = {
    statement55: t("card.presets.statement55"),
    sber120: t("card.presets.sber120"),
    year: t("card.presets.year"),
    gpb180: t("card.presets.gpb180"),
    vtb110: t("card.presets.vtb110"),
  };
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-md" data-testid="card-terms-dialog">
        <DialogHeader>
          <DialogTitle>{t("card.termsTitle")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1">
            <Label>{t("card.preset")}</Label>
            <div className="flex flex-wrap gap-2">
              {Object.entries(PRESETS).map(([key, p]) => (
                <Button key={key} type="button" size="sm" variant="outline" onClick={() => set(p)}>
                  {presetName[key]}
                </Button>
              ))}
            </div>
            <p className="text-xs text-muted-foreground">{t("card.presetHint")}</p>
          </div>
          {field("limit", t("card.limit", { currency: account.currency }))}
          {field("statementDay", t("card.statementDay"), t("card.statementDayHint"))}
          {toggle("payByPeriodEnd", t("card.payByPeriodEnd"), t("card.payByPeriodEndHint"))}
          {!f.payByPeriodEnd && field("paymentDays", t("card.paymentDays"), t("card.paymentDaysHint"))}
          <div className="grid gap-1">
            <Label>{t("card.graceKind")}</Label>
            <Select value={f.kind} onValueChange={(v) => set({ kind: v as Form["kind"] })}>
              <SelectTrigger aria-label={t("card.graceKind")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="statement">{t("card.kindStatement")}</SelectItem>
                <SelectItem value="long">{t("card.kindLong")}</SelectItem>
                <SelectItem value="windows">{t("card.kindWindows")}</SelectItem>
                <SelectItem value="running">{t("card.kindRunning")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {(f.kind === "long" || f.kind === "running") && field("graceDays", t("card.graceDays"))}
          {f.kind === "running" && (
            <div className="grid gap-1">
              <Label>{t("card.runFrom")}</Label>
              <Select value={f.runFrom} onValueChange={(v) => set({ runFrom: v as Form["runFrom"] })}>
                <SelectTrigger aria-label={t("card.runFrom")}>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="purchase">{t("card.runFromPurchase")}</SelectItem>
                  <SelectItem value="next_day">{t("card.runFromNextDay")}</SelectItem>
                  <SelectItem value="month_start">{t("card.runFromMonthStart")}</SelectItem>
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">{t("card.runFromHint")}</p>
            </div>
          )}
          {f.kind === "windows" && (
            <>
              {field("windowMonths", t("card.windowMonths"))}
              {field("graceMonths", t("card.graceMonths"), t("card.graceMonthsHint"))}
              {field("openedOn", t("card.openedOn"), t("card.openedOnHint"), "date")}
            </>
          )}
          {toggle("graceAllLost", t("card.graceAllLost"), t("card.graceAllLostHint"))}
          {field("minPercent", t("card.minPercent"))}
          {field("minFloor", t("card.minFloor", { currency: account.currency }))}
          {toggle("chargesInFull", t("card.chargesInFull"), t("card.chargesInFullHint"))}
          <div className="grid gap-1" data-testid="card-transfer-categories">
            <Label htmlFor="card-transfer-add">{t("card.transferCategories")}</Label>
            {f.transferCategories.length > 0 && (
              <div className="flex flex-wrap gap-1">
                {f.transferCategories.map((id) => (
                  <Button
                    key={id}
                    type="button"
                    size="sm"
                    variant="secondary"
                    aria-label={t("card.transferRemove", { name: categoryLabel(categories.data ?? [], id) ?? "" })}
                    onClick={() => set({ transferCategories: f.transferCategories.filter((x) => x !== id) })}
                  >
                    {categoryLabel(categories.data ?? [], id) ?? "?"} ×
                  </Button>
                ))}
              </div>
            )}
            <CategorySelect
              id="card-transfer-add"
              categories={categories.data ?? []}
              kind="expense"
              value={null}
              onChange={(id) => id && !f.transferCategories.includes(id) && set({ transferCategories: [...f.transferCategories, id] })}
            />
            <p className="text-xs text-muted-foreground">{t("card.transferCategoriesHint")}</p>
          </div>
          <fieldset className="grid gap-2 rounded-md border p-3" data-testid="card-fees">
            <legend className="px-1 text-sm font-medium">{t("card.feesTitle")}</legend>
            <p className="text-xs text-muted-foreground">{t("card.feesHint")}</p>
            {field("monthlyFee", t("card.monthlyFee", { currency: account.currency }), t("card.monthlyFeeHint"))}
            {field("cashFree", t("card.cashFree", { currency: account.currency }))}
            <div className="grid grid-cols-2 gap-2">
              {field("cashPercent", t("card.cashPercent"))}
              {field("cashFixed", t("card.feeFixed", { currency: account.currency }))}
            </div>
            <div className="grid grid-cols-2 gap-2">
              {field("transferPercent", t("card.transferPercent"))}
              {field("transferFixed", t("card.feeFixedTransfer", { currency: account.currency }))}
            </div>
            {field("penaltyDaily", t("card.penaltyDaily"))}
          </fieldset>
          <fieldset className="grid gap-2 rounded-md border p-3" data-testid="card-cashback-rules">
            <legend className="px-1 text-sm font-medium">{t("card.cashbackTitle")}</legend>
            <p className="text-xs text-muted-foreground">{t("card.cashbackHint")}</p>
            <div className="grid grid-cols-2 gap-2">
              {field("cashbackBase", t("card.cashbackBase"))}
              {field("cashbackCap", t("card.cashbackCap", { currency: account.currency }))}
            </div>
            {f.cashbackCategories.map((c, i) => (
              <div key={c.id} className="flex items-end gap-2">
                <div className="grid flex-1 gap-1">
                  <Label htmlFor={`card-cashback-${c.id}`}>{t("card.cashbackIn", { name: categoryLabel(categories.data ?? [], c.id) ?? "?" })}</Label>
                  <Input
                    id={`card-cashback-${c.id}`}
                    inputMode="decimal"
                    value={c.percent}
                    onChange={(e) =>
                      set({ cashbackCategories: f.cashbackCategories.map((x, j) => (j === i ? { ...x, percent: e.target.value } : x)) })
                    }
                  />
                </div>
                <Button
                  type="button"
                  size="sm"
                  variant="ghost"
                  aria-label={t("card.transferRemove", { name: categoryLabel(categories.data ?? [], c.id) ?? "" })}
                  onClick={() => set({ cashbackCategories: f.cashbackCategories.filter((_, j) => j !== i) })}
                >
                  ×
                </Button>
              </div>
            ))}
            <div className="grid gap-1">
              <Label htmlFor="card-cashback-add">{t("card.cashbackAdd")}</Label>
              <CategorySelect
                id="card-cashback-add"
                categories={categories.data ?? []}
                kind="expense"
                value={null}
                onChange={(id) =>
                  id && !f.cashbackCategories.some((c) => c.id === id) && set({ cashbackCategories: [...f.cashbackCategories, { id, percent: "5" }] })
                }
              />
            </div>
            {field("cashbackDays", t("card.cashbackDays"))}
            {toggle("cashbackPoints", t("card.cashbackPointsLabel"), t("card.cashbackPointsHint"))}
          </fieldset>
          {field("rate", t("card.rate"))}
          {field("ownRate", t("card.ownRate"), t("card.ownRateHint"))}
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("card.failed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
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
