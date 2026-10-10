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
import {
  useCreditCard,
  useDeleteBankFigures,
  useDeleteInstallment,
  useSetBankFigures,
  useSetCreditCard,
  useSetInstallment,
  type CreditCard,
  type CreditCardTerms,
} from "@/api/credit-cards";
import { useOperations } from "@/api/operations";
import { formatMinor, minorToInput, parseToMinor } from "@/lib/money";
import { formatDate, localToday } from "@/lib/dates";
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
  // What the bank itself says stands over the reckoning until its day
  // (decision Р-32).
  const bankMinimum = st.bank?.minimum ?? null;
  const bankGrace = st.bank?.grace ?? null;
  const minimumAmount = bankMinimum ? bankMinimum.left_minor : st.minimum_minor;
  const minimumOn = bankMinimum ? bankMinimum.on : st.minimum_on;
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
          <div className="text-sm text-muted-foreground">
            {bankMinimum ? t("card.minimumBank") : st.minimum_estimate ? t("card.minimumEstimate") : t("card.minimum")}
          </div>
          <div
            className={cn("text-xl font-semibold tabular-nums", !bankMinimum && st.minimum_missed && "text-red-700 dark:text-red-400")}
            data-testid="card-minimum"
          >
            {minimumAmount > 0 ? formatMinor(minimumAmount, c) : t("card.minimumPaid")}
          </div>
          {minimumAmount > 0 && (
            <div className={cn("text-xs", !bankMinimum && st.minimum_missed ? "text-red-700 dark:text-red-400" : "text-muted-foreground")}>
              {!bankMinimum && st.minimum_missed ? t("card.minimumMissed", { date: formatDate(minimumOn) }) : t("card.by", { date: formatDate(minimumOn) })}
            </div>
          )}
          {st.minimum_overdue_minor > 0 && !st.minimum_missed && (
            <div className="text-xs text-red-700 dark:text-red-400" data-testid="card-minimum-overdue">
              {t("card.minimumOverdue", { amount: formatMinor(st.minimum_overdue_minor, c) })}
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
          {bankGrace ? (
            <div className={cn(daysUntil(bankGrace.on) <= SOON_DAYS && "font-medium text-amber-700 dark:text-amber-400")}>
              {t("card.graceBank", { amount: formatMinor(bankGrace.left_minor, c), date: formatDate(bankGrace.on) })}
            </div>
          ) : next ? (
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

      <BankBlock account={account} card={data} canEdit={canEdit} />

      {data.by_journal && (st.installments.length > 0 || canEdit) && (
        <InstallmentsBlock account={account} card={data} canEdit={canEdit} />
      )}

      {data.benefit && <BenefitBlock benefit={data.benefit} currency={c} ownRate={data.terms.own_rate} />}

      <div className="text-xs text-muted-foreground">
        {t("card.termsLine", {
          day: data.terms.statement_day,
          pay: data.terms.pay_by_period_end
            ? t("card.payByPeriodEndShort")
            : data.terms.pay_day > 0
              ? t("card.payDayShort", { day: data.terms.pay_day })
              : t("card.payDays", { days: data.terms.payment_days }),
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

// BankBlock sets what the bank itself says is due against the card's own
// reckoning (decision Р-32): a gap is a rule of the bank's the terms do not
// tell.
function BankBlock({ account, card, canEdit }: { account: AccountWithBalance; card: CreditCard; canEdit: boolean }) {
  const { t } = useTranslation();
  const [editing, setEditing] = useState(false);
  const b = card.status.bank;
  const c = account.currency;
  const line = (label: string, d: NonNullable<NonNullable<typeof b>["grace"]>) => (
    <div className={d.agrees ? "text-muted-foreground" : "text-amber-700 dark:text-amber-400"}>
      {t("card.bankLine", { what: label, amount: formatMinor(d.amount_minor, c), date: formatDate(d.on) })}{" "}
      {d.agrees
        ? t("card.bankAgrees")
        : d.ours
          ? t("card.bankDiffers", { amount: formatMinor(d.ours.amount_minor, c), date: formatDate(d.ours.on) })
          : t("card.bankNoOurs")}
    </div>
  );
  if (!b && !canEdit) return null;
  return (
    <div className="grid gap-1 text-sm" data-testid="card-bank">
      {b && (
        <>
          <div className="font-medium">{t("card.bankTitle", { date: formatDate(b.stated_on) })}</div>
          {b.grace && line(t("card.bankGrace"), b.grace)}
          {b.minimum && line(t("card.bankMinimum"), b.minimum)}
          {[b.grace, b.minimum].some((d) => d && !d.agrees) && <div className="text-xs text-muted-foreground">{t("card.bankGapHint")}</div>}
        </>
      )}
      {canEdit && (
        <Button type="button" size="sm" variant="outline" className="justify-self-start" onClick={() => setEditing(true)}>
          {t("card.bankEdit")}
        </Button>
      )}
      {editing && <BankDialog account={account} card={card} onClose={() => setEditing(false)} />}
    </div>
  );
}

function BankDialog({ account, card, onClose }: { account: AccountWithBalance; card: CreditCard; onClose: () => void }) {
  const { t } = useTranslation();
  const save = useSetBankFigures(account.id);
  const forget = useDeleteBankFigures(account.id);
  const st = card.status;
  const [statedOn, setStatedOn] = useState(localToday);
  const [grace, setGrace] = useState(st.bank?.grace ? minorToInput(st.bank.grace.amount_minor) : "");
  const [graceOn, setGraceOn] = useState(st.bank?.grace?.on ?? st.grace[0]?.on ?? "");
  const [minimum, setMinimum] = useState(st.bank?.minimum ? minorToInput(st.bank.minimum.amount_minor) : "");
  const [minimumOn, setMinimumOn] = useState(st.bank?.minimum?.on ?? st.minimum_on ?? "");
  const graceMinor = grace.trim() === "" ? null : parseToMinor(grace);
  const minimumMinor = minimum.trim() === "" ? null : parseToMinor(minimum);
  const isDay = (s: string) => /^\d{4}-\d{2}-\d{2}$/.test(s);
  const valid =
    isDay(statedOn) && (graceMinor !== null || minimumMinor !== null) &&
    (grace.trim() === "" || (graceMinor !== null && graceMinor >= 0 && isDay(graceOn))) &&
    (minimum.trim() === "" || (minimumMinor !== null && minimumMinor >= 0 && isDay(minimumOn)));
  const submit = () => {
    if (!valid) return;
    save.mutate(
      {
        stated_on: statedOn,
        grace: graceMinor !== null ? { on: graceOn, amount_minor: graceMinor } : null,
        minimum: minimumMinor !== null ? { on: minimumOn, amount_minor: minimumMinor } : null,
      },
      { onSuccess: onClose },
    );
  };
  const pair = (id: string, label: string, amount: string, setAmount: (v: string) => void, on: string, setOn: (v: string) => void, hint?: string) => (
    <div className="grid gap-1">
      <div className="grid grid-cols-2 gap-2">
        <div className="grid gap-1">
          <Label htmlFor={`bank-${id}`}>{label}</Label>
          <Input id={`bank-${id}`} inputMode="decimal" value={amount} placeholder={hint} onChange={(e) => setAmount(e.target.value)} />
        </div>
        <div className="grid gap-1">
          <Label htmlFor={`bank-${id}-on`}>{t("card.bankBy")}</Label>
          <Input id={`bank-${id}-on`} type="date" value={on} onChange={(e) => setOn(e.target.value)} />
        </div>
      </div>
    </div>
  );
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-md" data-testid="card-bank-dialog">
        <DialogHeader>
          <DialogTitle>{t("card.bankEdit")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <p className="text-xs text-muted-foreground">{t("card.bankHint")}</p>
          {pair("grace", t("card.bankGraceInput", { currency: account.currency }), grace, setGrace, graceOn, setGraceOn,
            st.grace[0] ? minorToInput(st.grace[0].amount_minor) : undefined)}
          {pair("minimum", t("card.bankMinimumInput", { currency: account.currency }), minimum, setMinimum, minimumOn, setMinimumOn,
            st.minimum_minor > 0 ? minorToInput(st.minimum_minor) : undefined)}
          <div className="grid gap-1">
            <Label htmlFor="bank-stated">{t("card.bankStatedOn")}</Label>
            <Input id="bank-stated" type="date" value={statedOn} onChange={(e) => setStatedOn(e.target.value)} />
          </div>
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("card.failed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          {st.bank && (
            <Button variant="ghost" disabled={forget.isPending} onClick={() => forget.mutate(undefined, { onSuccess: onClose })}>
              {t("card.bankForget")}
            </Button>
          )}
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

// InstallmentsBlock is the card's purchases in installments (decision Р-33):
// what is left of each and its next part, the parts due now, and a way to put
// a purchase in installments.
function InstallmentsBlock({ account, card, canEdit }: { account: AccountWithBalance; card: CreditCard; canEdit: boolean }) {
  const { t } = useTranslation();
  const remove = useDeleteInstallment(account.id);
  const [adding, setAdding] = useState(false);
  const st = card.status;
  const c = account.currency;
  return (
    <div className="grid gap-1 text-sm" data-testid="card-installments">
      {st.installments.length > 0 && <div className="font-medium">{t("card.installmentsTitle")}</div>}
      {st.installments.map((i) => (
        <div key={i.operation_id} className="flex flex-wrap items-baseline justify-between gap-2" data-testid="card-installment">
          <span>
            {t("card.installmentLine", {
              date: formatDate(i.on),
              what: i.note || t("card.installmentPurchase"),
              amount: formatMinor(i.amount_minor, c),
              months: i.plan.months,
              left: formatMinor(i.left_minor, c),
              next: formatMinor(i.next_minor, c),
            })}
          </span>
          {canEdit && (
            <Button
              type="button"
              size="sm"
              variant="ghost"
              aria-label={t("card.installmentRemove")}
              disabled={remove.isPending}
              onClick={() => remove.mutate(i.operation_id)}
            >
              ×
            </Button>
          )}
        </div>
      ))}
      {st.installments_due_minor > 0 && (
        <div className="text-muted-foreground">{t("card.installmentsDue", { amount: formatMinor(st.installments_due_minor, c) })}</div>
      )}
      {canEdit && (
        <Button type="button" size="sm" variant="outline" className="justify-self-start" onClick={() => setAdding(true)}>
          {t("card.installmentAdd")}
        </Button>
      )}
      {adding && <InstallmentDialog account={account} card={card} onClose={() => setAdding(false)} />}
    </div>
  );
}

function InstallmentDialog({ account, card, onClose }: { account: AccountWithBalance; card: CreditCard; onClose: () => void }) {
  const { t } = useTranslation();
  const save = useSetInstallment(account.id);
  // The purchases of the last four months, worked out once.
  const [since] = useState(() => new Date(Date.now() - 120 * 86_400_000).toISOString().slice(0, 10));
  const ops = useOperations(account.id, 200, { type: "withdrawal", from: since });
  const taken = new Set(card.status.installments.map((i) => i.operation_id));
  const purchases = (ops.data?.pages[0]?.operations ?? []).filter(
    (o) => o.amount_minor < 0 && !o.transfer_group_id && !taken.has(o.id),
  );
  const [operationId, setOperationId] = useState("");
  const [months, setMonths] = useState(String(card.terms.installment.months || 6));
  const [feePercent, setFeePercent] = useState(card.terms.installment.monthly_fee_percent || "0");
  const [fee, setFee] = useState(minorToInput(card.terms.installment.fee_minor));
  const feeMinor = parseToMinor(fee.trim() === "" ? "0" : fee);
  const valid =
    operationId !== "" && /^\d+$/.test(months) && Number(months) >= 1 && Number(months) <= 60 &&
    !Number.isNaN(Number(feePercent.replace(",", "."))) && feeMinor !== null && feeMinor >= 0;
  const submit = () => {
    if (!valid || feeMinor === null) return;
    save.mutate(
      { operationId, plan: { months: Number(months), monthly_fee_percent: String(Number(feePercent.replace(",", "."))), fee_minor: feeMinor } },
      { onSuccess: onClose },
    );
  };
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-md" data-testid="card-installment-dialog">
        <DialogHeader>
          <DialogTitle>{t("card.installmentAdd")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1">
            <Label>{t("card.installmentWhich")}</Label>
            <Select value={operationId} onValueChange={setOperationId}>
              <SelectTrigger aria-label={t("card.installmentWhich")}>
                <SelectValue placeholder={purchases.length ? t("card.installmentPick") : t("card.installmentNone")} />
              </SelectTrigger>
              <SelectContent className="max-h-80">
                {purchases.map((o) => (
                  <SelectItem key={o.id} value={o.id}>
                    {formatDate(o.occurred_on)} · {formatMinor(-o.amount_minor, account.currency)}
                    {o.note || o.counterparty ? ` · ${o.note || o.counterparty}` : ""}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-1">
            <Label htmlFor="inst-months">{t("card.installmentMonths")}</Label>
            <Input id="inst-months" inputMode="numeric" value={months} onChange={(e) => setMonths(e.target.value)} />
          </div>
          <div className="grid grid-cols-2 gap-2">
            <div className="grid gap-1">
              <Label htmlFor="inst-fee-percent">{t("card.installmentFeePercent")}</Label>
              <Input id="inst-fee-percent" inputMode="decimal" value={feePercent} onChange={(e) => setFeePercent(e.target.value)} />
            </div>
            <div className="grid gap-1">
              <Label htmlFor="inst-fee">{t("card.installmentFee", { currency: account.currency })}</Label>
              <Input id="inst-fee" inputMode="decimal" value={fee} onChange={(e) => setFee(e.target.value)} />
            </div>
          </div>
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
const LENIENT = { graceAllLost: false, missedMinimumPeriod: false, dueMode: "days" as const, chargesInFull: false, minRound: "0" };
const PRESETS: Record<string, Partial<Form>> = {
  statement55: { kind: "statement", paymentDays: "25", minPercent: "8", ...LENIENT, missedMinimumPeriod: true },
  sber120: { kind: "long", graceDays: "120", paymentDays: "20", minPercent: "3", ...LENIENT, chargesInFull: true },
  year: { kind: "long", graceDays: "365", paymentDays: "20", minPercent: "3", ...LENIENT },
  // «Халва» (decision Р-33): every purchase in installments, 99 ₽ with the
  // first part; no interest; the payment is the parts.
  halva: {
    kind: "statement", paymentDays: "15", statementDay: "1", minPercent: "0", minFloor: "0", rate: "0", ...LENIENT,
    installMonths: "3", installFeePercent: "0", installFee: "99",
  },
  // ВТБ «Карта возможностей» (#457): one grace of 110 days from the 1st of
  // the first purchase's month, renewed once the debt is repaid; the minimum
  // by the 20th.
  vtb110: {
    kind: "running", graceDays: "110", runFrom: "month_start", statementDay: "1", minPercent: "3",
    minFloor: "0", ...LENIENT, dueMode: "day", payDay: "20", minRound: "100",
  },
  // Газпромбанк «180 дней» (decision Р-28): two months of purchases, paid by
  // the end of the sixth; the minimum by the end of the next month.
  gpb180: {
    kind: "windows", windowMonths: "2", graceMonths: "6", statementDay: "1", minPercent: "3", minFloor: "500",
    rate: "59.99", graceAllLost: true, dueMode: "periodEnd", chargesInFull: true, minRound: "0",
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
  missedMinimumPeriod: boolean;
  dueMode: "days" | "periodEnd" | "day";
  payDay: string;
  minRound: string;
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
  installMonths: string;
  installFeePercent: string;
  installFee: string;
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
    missedMinimumPeriod: terms?.missed_minimum_period ?? false,
    dueMode: terms?.pay_by_period_end ? "periodEnd" : terms && terms.pay_day > 0 ? "day" : "days",
    payDay: terms && terms.pay_day > 0 ? String(terms.pay_day) : "20",
    minRound: terms ? minorToInput(terms.min_round_up_minor) : "0",
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
    installMonths: terms ? String(terms.installment.months) : "0",
    installFeePercent: terms?.installment.monthly_fee_percent ?? "0",
    installFee: terms ? minorToInput(terms.installment.fee_minor) : "0",
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
    int(f.statementDay, 1, 31) && (f.dueMode !== "days" || int(f.paymentDays, 0, 60)) &&
    (f.dueMode !== "day" || int(f.payDay, 1, 31)) && money(f.minRound) !== null &&
    ((f.kind !== "long" && f.kind !== "running") || int(f.graceDays, 1, 1100)) &&
    (f.kind !== "windows" ||
      (int(f.windowMonths, 1, 12) && int(f.graceMonths, Number(f.windowMonths), 36) && /^\d{4}-\d{2}-\d{2}$/.test(f.openedOn))) &&
    decimalOk(f.minPercent, 100.0001) && decimalOk(f.rate, 1000) && (f.ownRate.trim() === "" || decimalOk(f.ownRate, 1000)) &&
    feesValid && cashbackValid && int(f.installMonths || "0", 0, 60) && decimalOk(f.installFeePercent || "0", 100) &&
    money(f.installFee) !== null;

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
        missed_minimum_period: f.missedMinimumPeriod,
        pay_by_period_end: f.dueMode === "periodEnd",
        pay_day: f.dueMode === "day" ? Number(f.payDay) : 0,
        min_round_up_minor: money(f.minRound) ?? 0,
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
        installment: {
          months: Number(f.installMonths || "0"),
          monthly_fee_percent: String(num(f.installFeePercent || "0")),
          fee_minor: money(f.installFee) ?? 0,
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
  type Switch = "graceAllLost" | "missedMinimumPeriod" | "chargesInFull" | "cashbackPoints";
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
    halva: t("card.presets.halva"),
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
          <div className="grid gap-1">
            <Label>{t("card.dueMode")}</Label>
            <Select value={f.dueMode} onValueChange={(v) => set({ dueMode: v as Form["dueMode"] })}>
              <SelectTrigger aria-label={t("card.dueMode")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="days">{t("card.dueDays")}</SelectItem>
                <SelectItem value="periodEnd">{t("card.payByPeriodEnd")}</SelectItem>
                <SelectItem value="day">{t("card.dueDay")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {f.dueMode === "days" && field("paymentDays", t("card.paymentDays"), t("card.paymentDaysHint"))}
          {f.dueMode === "day" && field("payDay", t("card.payDay"))}
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
          {toggle("missedMinimumPeriod", t("card.missedMinimumPeriod"), t("card.missedMinimumPeriodHint"))}
          {field("minPercent", t("card.minPercent"))}
          {field("minFloor", t("card.minFloor", { currency: account.currency }))}
          {field("minRound", t("card.minRound", { currency: account.currency }), t("card.minRoundHint"))}
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
          <fieldset className="grid gap-2 rounded-md border p-3" data-testid="card-installment-rules">
            <legend className="px-1 text-sm font-medium">{t("card.installmentTitle")}</legend>
            {field("installMonths", t("card.installAll"), t("card.installAllHint"))}
            <div className="grid grid-cols-2 gap-2">
              {field("installFeePercent", t("card.installmentFeePercent"))}
              {field("installFee", t("card.installmentFee", { currency: account.currency }))}
            </div>
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
