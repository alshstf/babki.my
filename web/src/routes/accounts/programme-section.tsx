import { useMemo, useState } from "react";
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
  useDeleteMark,
  useDeleteProgramme,
  useDepositProgrammes,
  useSetMark,
  useSetProgramme,
  type AccountRates,
  type DepositProgramme,
  type ProgrammeCondition,
} from "@/api/account-rates";
import { formatMinor, minorToInput, parseToMinor } from "@/lib/money";
import { formatDate, localToday } from "@/lib/dates";

// The program measures the conditions (purchases, wages, a balance) in roubles.
const RUB = "RUB";

// A rate as the API writes it ("15.500"), as people read it ("15,5 %").
export function rate(s: string): string {
  return `${Number(s).toLocaleString("ru-RU", { maximumFractionDigits: 3 })} %`;
}

// The first day of this month, the day a mark made now holds from.
function thisMonth(): string {
  return `${localToday().slice(0, 8)}01`;
}

// QuoteLine says where the programme's rate comes from: the bank's terms of
// the edition, the welcome period, the bonuses that counted, the parts of a
// balance above a cap — in the account's currency.
export function QuoteLine({ data, currency }: { data: AccountRates; currency: string }) {
  const { t } = useTranslation();
  const q = data.quote;
  const p = data.programme;
  if (!q || !p) return null;
  const named = (id: string) => data.conditions.find((c) => c.id === id)?.name ?? id;
  return (
    <div className="grid gap-0.5 text-sm text-muted-foreground" data-testid="account-rate-quote">
      <div>
        {t("programme.byTerms", { bank: p.bank_name, name: p.programme_name, variant: p.variant_name, date: formatDate(q.edition_from) })}
      </div>
      {q.period && <div className="text-green-700 dark:text-green-400">{t("programme.period", { name: q.period })}</div>}
      {q.bonuses.length > 0 && <div>{t("programme.bonuses", { names: q.bonuses.map(named).join(", ") })}</div>}
      {q.parts.length > 1 && (
        <div>{q.parts.map((part) => t("programme.part", { from: formatMinor(part.from_minor, currency), to: formatMinor(part.to_minor, currency), percent: rate(part.percent) })).join(" · ")}</div>
      )}
    </div>
  );
}

// ProgrammeBlock is the account's programme of its bank (Р-53 Б): what was
// chosen at opening, and its conditions with the family's marks for this
// month — a check, or a step of a bonus.
export function ProgrammeBlock({ account, data, canEdit }: { account: AccountWithBalance; data: AccountRates; canEdit: boolean }) {
  const { t } = useTranslation();
  const [editing, setEditing] = useState(false);
  const untie = useDeleteProgramme(account.id);
  const p = data.programme;
  if (!p) {
    return (
      <div className="grid gap-2" data-testid="account-programme">
        <p className="text-sm text-muted-foreground">{t("programme.none")}</p>
        {canEdit && (
          <div>
            <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
              {t("programme.choose")}
            </Button>
          </div>
        )}
        {editing && <ProgrammeDialog account={account} data={data} onClose={() => setEditing(false)} />}
      </div>
    );
  }
  const unit = p.term_unit === "months" ? t("programme.months") : t("programme.days");
  return (
    <div className="grid gap-2" data-testid="account-programme">
      <div className="text-sm">
        <div className="font-medium">
          {p.bank_name} · {p.programme_name}
        </div>
        <div className="text-muted-foreground">
          {p.variant_name} · {t("programme.openedOn", { date: formatDate(p.opened_on) })}
          {p.term !== null && ` · ${t("programme.term", { term: p.term, unit })}`}
          {p.amount_minor !== null && ` · ${formatMinor(p.amount_minor, account.currency)}`}
        </div>
        <div className="text-muted-foreground">{p.conditions === "auto" ? t("programme.modeAuto") : t("programme.modeManual")}</div>
        {data.quote === null && <div className="text-amber-700 dark:text-amber-400">{t("programme.noQuote")}</div>}
      </div>
      {data.conditions.length > 0 && (
        <div className="grid gap-1.5" data-testid="account-programme-conditions">
          <div className="text-sm font-medium">{t("programme.conditions")}</div>
          {data.conditions.map((c) => (
            <ConditionRow key={c.id} account={account} data={data} condition={c} canEdit={canEdit} />
          ))}
          <p className="text-xs text-muted-foreground">{p.conditions === "auto" ? t("programme.autoHint") : t("programme.marksHint")}</p>
        </div>
      )}
      {canEdit && (
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
            {t("programme.change")}
          </Button>
          <Button variant="ghost" size="sm" disabled={untie.isPending} onClick={() => untie.mutate()}>
            {t("programme.untie")}
          </Button>
        </div>
      )}
      {editing && <ProgrammeDialog account={account} data={data} onClose={() => setEditing(false)} />}
    </div>
  );
}

// ConditionRow is a condition with how it stands this month: a check for
// one of one level, a choice of steps for one of several. In the automatic
// mode, what the data told — or that they told nothing — and a mark of the
// family's, which stands over them, can be taken back.
function ConditionRow({ account, data, condition: c, canEdit }: { account: AccountWithBalance; data: AccountRates; condition: ProgrammeCondition; canEdit: boolean }) {
  const { t } = useTranslation();
  const mark = useSetMark(account.id);
  const unmark = useDeleteMark(account.id);
  const auto = data.programme?.conditions === "auto" && c.check !== "manual";
  const plus = c.plus !== null ? ` (+${rate(c.plus)})` : "";
  const busy = !canEdit || mark.isPending || unmark.isPending;
  const month = c.measured_month ? monthName(c.measured_month) : "";
  const latest = [...data.marks].filter((m) => m.condition === c.id).sort((a, b) => b.from_on.localeCompare(a.from_on))[0];
  const note = !auto ? null : c.told === "data" ? (
    <span className="text-xs text-muted-foreground">
      {c.check === "card_spend"
        ? t("programme.toldSpend", { month, amount: formatMinor(c.measured_minor ?? 0, RUB) })
        : c.check === "wages"
          ? t("programme.toldWages", { month, amount: formatMinor(c.measured_minor ?? 0, RUB) })
          : t("programme.toldBalance", { month, amount: formatMinor(c.measured_minor ?? 0, RUB) })}
    </span>
  ) : c.told === "mark" ? (
    <span className="text-xs text-muted-foreground">
      {t("programme.toldMark")}
      {canEdit && latest && (
        <>
          {" "}
          <button type="button" className="underline" disabled={busy} onClick={() => unmark.mutate({ condition: c.id, fromOn: latest.from_on })}>
            {t("programme.backToData")}
          </button>
        </>
      )}
    </span>
  ) : (
    <span className="text-xs text-amber-700 dark:text-amber-400">
      {c.check === "bank_balance" ? t("programme.noBalance", { month }) : t("programme.noData", { month })}
    </span>
  );
  if (c.levels.length === 0) {
    const id = `cond-${c.id}`;
    return (
      <div className="grid gap-0.5">
        <div className="flex items-start gap-2 text-sm">
          <Checkbox
            id={id}
            checked={c.met}
            disabled={busy}
            onCheckedChange={(v) => mark.mutate({ condition: c.id, from_on: thisMonth(), level: 0, met: v === true })}
          />
          <Label htmlFor={id} className="font-normal leading-snug">
            {c.name}
            {plus}
          </Label>
        </div>
        {note && <div className="pl-6">{note}</div>}
      </div>
    );
  }
  const value = c.met ? String(c.level) : "none";
  // A step told by a sum shows the sum; one told by a mark — its number.
  const step = (from: number, i: number) => (c.check === "manual" ? t("programme.level", { n: i + 1 }) : t("programme.from", { amount: formatMinor(from, RUB) }));
  return (
    <div className="grid gap-0.5">
      <div className="grid gap-1 text-sm sm:grid-cols-[1fr_auto] sm:items-center sm:gap-3">
        <span>{c.name}</span>
        <Select
          value={value}
          disabled={busy}
          onValueChange={(v) => mark.mutate({ condition: c.id, from_on: thisMonth(), level: v === "none" ? 0 : Number(v), met: v !== "none" })}
        >
          <SelectTrigger className="h-8 w-full sm:w-56" aria-label={c.name}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="none">{t("programme.notMet")}</SelectItem>
            {c.levels.map((l, i) => (
              <SelectItem key={l.from} value={String(i)}>
                {step(l.from, i)} (+{rate(l.plus)})
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {note}
    </div>
  );
}

// monthName is a month YYYY-MM as a word in the prepositional case: «сентябрь
// 2026» → «сентябрь 2026», for «по операциям за …».
function monthName(m: string): string {
  const [y, mm] = m.split("-").map(Number);
  return new Date(Date.UTC(y, mm - 1, 1)).toLocaleDateString("ru-RU", { month: "long", year: "numeric", timeZone: "UTC" }).replace(" г.", "");
}

// ProgrammeDialog ties the account to a programme of the catalog: its bank's
// first, the variant, the day of opening and, for a deposit, the term and
// sum the bank has a rate for. Only the variants in the account's currency.
function ProgrammeDialog({ account, data, onClose }: { account: AccountWithBalance; data: AccountRates; onClose: () => void }) {
  const { t } = useTranslation();
  const catalog = useDepositProgrammes();
  const save = useSetProgramme(account.id);
  const kind = account.type === "deposit" ? "deposit" : "savings";
  const programmes = useMemo(() => {
    const own = (catalog.data ?? [])
      .filter((p) => p.kind === kind)
      .map((p) => ({ ...p, variants: p.variants.filter((v) => v.currency === account.currency) }))
      .filter((p) => p.variants.length > 0);
    const bank = account.intermediary_id ?? null;
    return own.sort((a, b) => Number(b.bank_id === bank) - Number(a.bank_id === bank) || a.bank_name.localeCompare(b.bank_name, "ru"));
  }, [catalog.data, kind, account.intermediary_id, account.currency]);
  const was = data.programme;
  const [programmeId, setProgrammeId] = useState(was?.programme_id ?? "");
  const programme: DepositProgramme | undefined = programmes.find((p) => p.id === programmeId);
  const [variantId, setVariantId] = useState(was?.variant_id ?? "");
  const variant = programme?.variants.find((v) => v.id === variantId) ?? programme?.variants[0];
  const [openedOn, setOpenedOn] = useState(was?.opened_on ?? localToday());
  const [termText, setTermText] = useState(was?.term !== null && was?.term !== undefined ? String(was.term) : "");
  const [amountText, setAmountText] = useState(
    was?.amount_minor !== null && was?.amount_minor !== undefined ? minorToInput(was.amount_minor) : account.balance ? minorToInput(account.balance.amount_minor) : "",
  );
  const term = Number(termText);
  const amount = parseToMinor(amountText);
  const termOK = kind === "savings" || (Number.isInteger(term) && (variant?.terms ?? []).some((r) => term >= r.from && term <= r.to));
  const amountOK = kind === "savings" || (amount !== null && amount >= (variant?.amount_from_minor ?? 0));
  const [mode, setMode] = useState<"manual" | "auto">(was?.conditions ?? "auto");
  const valid = !!programme && !!variant && /^\d{4}-\d{2}-\d{2}$/.test(openedOn) && termOK && amountOK;
  const unit = variant?.term_unit === "months" ? t("programme.months") : t("programme.days");
  const submit = () => {
    if (!valid || !programme || !variant) return;
    save.mutate(
      {
        programme_id: programme.id,
        variant_id: variant.id,
        opened_on: openedOn,
        term: kind === "deposit" ? term : null,
        amount_minor: kind === "deposit" ? amount : null,
        conditions: mode,
      },
      { onSuccess: onClose },
    );
  };
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-lg" data-testid="account-programme-dialog">
        <DialogHeader>
          <DialogTitle>{t("programme.dialogTitle", { name: account.name })}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          {catalog.isPending ? null : programmes.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t("programme.empty")}</p>
          ) : (
            <div className="grid gap-1">
              <Label htmlFor="programme-id">{t("programme.programme")}</Label>
              <Select
                value={programmeId}
                onValueChange={(v) => {
                  setProgrammeId(v);
                  setVariantId("");
                }}
              >
                <SelectTrigger id="programme-id">
                  <SelectValue placeholder={t("programme.pick")} />
                </SelectTrigger>
                <SelectContent>
                  {programmes.map((p) => (
                    <SelectItem key={p.id} value={p.id}>
                      {p.bank_name} · {p.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}
          {programme && (
            <>
              <div className="grid gap-1">
                <Label htmlFor="programme-variant">{t("programme.variant")}</Label>
                <Select value={variant?.id ?? ""} onValueChange={setVariantId}>
                  <SelectTrigger id="programme-variant">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {programme.variants.map((v) => (
                      <SelectItem key={v.id} value={v.id}>
                        {v.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="grid gap-1">
                <Label htmlFor="programme-opened">{kind === "deposit" ? t("programme.openedDeposit") : t("programme.opened")}</Label>
                <Input id="programme-opened" type="date" value={openedOn} onChange={(e) => setOpenedOn(e.target.value)} />
              </div>
              {kind === "deposit" && variant && (
                <div className="grid grid-cols-2 gap-2">
                  <div className="grid gap-1">
                    <Label htmlFor="programme-term">{t("programme.termLabel", { unit })}</Label>
                    <Input id="programme-term" inputMode="numeric" value={termText} onChange={(e) => setTermText(e.target.value.replace(/\D/g, ""))} />
                    <span className="text-xs text-muted-foreground">
                      {variant.terms.map((r) => (r.from === r.to ? String(r.from) : `${r.from}–${r.to}`)).join(", ")}
                    </span>
                  </div>
                  <div className="grid gap-1">
                    <Label htmlFor="programme-amount">{t("programme.amount", { currency: account.currency })}</Label>
                    <Input id="programme-amount" inputMode="decimal" value={amountText} onChange={(e) => setAmountText(e.target.value)} />
                    <span className="text-xs text-muted-foreground">{t("programme.amountFrom", { amount: formatMinor(variant.amount_from_minor, account.currency) })}</span>
                  </div>
                </div>
              )}
              {variant && variant.conditions.length > 0 && (
                <fieldset className="grid gap-1.5">
                  <legend className="mb-1 text-sm font-medium">{t("programme.mode")}</legend>
                  {(["auto", "manual"] as const).map((m) => (
                    <label key={m} className="flex items-start gap-2 text-sm">
                      <input type="radio" name="programme-mode" className="mt-1" checked={mode === m} onChange={() => setMode(m)} />
                      <span>
                        {m === "auto" ? t("programme.modeAutoChoice") : t("programme.modeManualChoice")}
                        <span className="block text-xs text-muted-foreground">{m === "auto" ? t("programme.modeAutoHint") : t("programme.modeManualHint")}</span>
                      </span>
                    </label>
                  ))}
                </fieldset>
              )}
              {programme.notes && <p className="text-xs text-muted-foreground">{programme.notes}</p>}
              <p className="text-xs text-muted-foreground">
                {t("programme.checked", { date: formatDate(programme.checked_on) })}{" "}
                {programme.sources.map((s, i) => (
                  <a key={s} href={s} target="_blank" rel="noreferrer" className="underline">
                    {t("programme.source", { n: i + 1 })}
                  </a>
                ))}
              </p>
            </>
          )}
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("programme.failed")}</AlertDescription>
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
