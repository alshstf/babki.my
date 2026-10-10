import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
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

      {st.lost.map((l) => (
        <Alert key={l.from} variant="destructive" data-testid="card-lost">
          <AlertDescription>
            {t("card.lost", {
              from: formatDate(l.from),
              to: formatDate(l.to),
              amount: formatMinor(l.amount_minor, c),
              interest: formatMinor(l.interest_minor, c),
            })}
          </AlertDescription>
        </Alert>
      ))}
      {st.non_grace_minor > 0 && (
        <p className="text-sm text-amber-700 dark:text-amber-400" data-testid="card-non-grace">
          {t("card.nonGrace", { amount: formatMinor(st.non_grace_minor, c), interest: formatMinor(st.non_grace_interest_minor, c) })}
        </p>
      )}

      {data.benefit && <BenefitBlock benefit={data.benefit} currency={c} ownRate={data.terms.own_rate} />}

      <div className="text-xs text-muted-foreground">
        {t("card.termsLine", {
          day: data.terms.statement_day,
          days: data.terms.payment_days,
          grace: data.terms.grace_kind === "long" ? t("card.graceLong", { days: data.terms.grace_days }) : t("card.graceStatement"),
          min: pct(data.terms.min_percent),
          floor: formatMinor(data.terms.min_floor_minor, c),
          rate: pct(data.terms.annual_rate),
        })}
      </div>
      <p className="text-xs text-muted-foreground">{t("card.hint")}</p>
      {canEdit && (
        <Button size="sm" variant="outline" className="justify-self-start" onClick={() => setEditing(true)}>
          {t("card.editTerms")}
        </Button>
      )}
      {editing && <TermsDialog account={account} card={data} onClose={() => setEditing(false)} />}
    </div>
  );
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
const PRESETS: Record<string, Partial<Form>> = {
  statement55: { kind: "statement", paymentDays: "25", minPercent: "8" },
  sber120: { kind: "long", graceDays: "120", paymentDays: "20", minPercent: "3" },
  year: { kind: "long", graceDays: "365", paymentDays: "20", minPercent: "3" },
};

interface Form {
  limit: string;
  statementDay: string;
  paymentDays: string;
  kind: CreditCardTerms["grace_kind"];
  graceDays: string;
  minPercent: string;
  minFloor: string;
  rate: string;
  ownRate: string;
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
    graceDays: terms && terms.grace_kind === "long" ? String(terms.grace_days) : "120",
    minPercent: terms?.min_percent ?? "3",
    minFloor: terms ? minorToInput(terms.min_floor_minor) : "300",
    rate: terms?.annual_rate ?? "",
    ownRate: terms?.own_rate ?? "",
  });
  const set = (patch: Partial<Form>) => setF((prev) => ({ ...prev, ...patch }));
  const num = (s: string) => Number(s.replace(",", "."));
  const int = (s: string, lo: number, hi: number) => /^\d+$/.test(s) && Number(s) >= lo && Number(s) <= hi;
  const limit = parseToMinor(f.limit);
  const floor = parseToMinor(f.minFloor || "0");
  const decimalOk = (s: string, hi: number) => s.trim() !== "" && !Number.isNaN(num(s)) && num(s) >= 0 && num(s) < hi;
  const valid =
    limit !== null && limit >= 0 && floor !== null && floor >= 0 &&
    int(f.statementDay, 1, 31) && int(f.paymentDays, 0, 60) &&
    (f.kind === "statement" || int(f.graceDays, 1, 1100)) &&
    decimalOk(f.minPercent, 100.0001) && decimalOk(f.rate, 1000) && (f.ownRate.trim() === "" || decimalOk(f.ownRate, 1000));

  const submit = () => {
    if (!valid || limit === null || floor === null) return;
    save.mutate(
      {
        limit_minor: limit,
        statement_day: Number(f.statementDay),
        payment_days: Number(f.paymentDays),
        grace_kind: f.kind,
        grace_days: f.kind === "long" ? Number(f.graceDays) : 0,
        min_percent: String(num(f.minPercent)),
        min_floor_minor: floor,
        annual_rate: String(num(f.rate)),
        own_rate: f.ownRate.trim() === "" ? null : String(num(f.ownRate)),
      },
      { onSuccess: onClose },
    );
  };
  const field = (id: keyof Form, label: string, hint?: string) => (
    <div className="grid gap-1">
      <Label htmlFor={`card-${id}`}>{label}</Label>
      <Input id={`card-${id}`} inputMode="decimal" value={f[id]} onChange={(e) => set({ [id]: e.target.value } as Partial<Form>)} />
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
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
                  {key === "statement55" ? t("card.presets.statement55") : key === "sber120" ? t("card.presets.sber120") : t("card.presets.year")}
                </Button>
              ))}
            </div>
            <p className="text-xs text-muted-foreground">{t("card.presetHint")}</p>
          </div>
          {field("limit", t("card.limit", { currency: account.currency }))}
          {field("statementDay", t("card.statementDay"), t("card.statementDayHint"))}
          {field("paymentDays", t("card.paymentDays"), t("card.paymentDaysHint"))}
          <div className="grid gap-1">
            <Label>{t("card.graceKind")}</Label>
            <Select value={f.kind} onValueChange={(v) => set({ kind: v as Form["kind"] })}>
              <SelectTrigger aria-label={t("card.graceKind")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="statement">{t("card.kindStatement")}</SelectItem>
                <SelectItem value="long">{t("card.kindLong")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {f.kind === "long" && field("graceDays", t("card.graceDays"))}
          {field("minPercent", t("card.minPercent"))}
          {field("minFloor", t("card.minFloor", { currency: account.currency }))}
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
