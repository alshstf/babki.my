import { Fragment, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useMembers } from "@/api/members";
import { useSession } from "@/api/session";
import {
  useDepositComparison,
  type DepositComparisonCondition,
  type DepositComparisonInput,
  type DepositComparisonRow,
  type DepositConditionChoice,
} from "@/api/deposit-comparison";
import { QueryGate } from "@/components/query-notice";
import { queryState } from "@/lib/query-state";
import { formatMinor, parseToMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { cn } from "@/lib/utils";

const RUB = "RUB";
const HORIZONS = [3, 6, 12, 18, 24, 36, 60] as const;
type Audience = NonNullable<DepositComparisonInput["audiences"]>[number];
const AUDIENCES: Audience[] = ["pensioner", "youth", "social", "salary", "premium", "bundle", "new", "regional"];
type Preset = "now" | "yes" | "no";

// A choice's key: the programme, the variant and the condition.
const keyOf = (programme: string, variant: string, condition: string) => `${programme}|${variant}|${condition}`;

// useDebounced is value once it has stood still for a moment: the sum and the
// key rate are typed figure by figure.
function useDebounced<T>(value: T, ms = 400): T {
  const [out, setOut] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setOut(value), ms);
    return () => clearTimeout(id);
  }, [value, ms]);
  return out;
}

// DepositsPage compares the catalog's deposits and savings accounts on a sum
// over a horizon (decision Р-57 Б): the key rate's scenario, the conditions —
// all at once and one by one on each row — what each brings, the tax it adds
// and what is left, and what of the sum deposit insurance covers.
export function DepositsPage() {
  const { t } = useTranslation();
  const session = useSession();
  const members = useMembers();
  const [amountText, setAmountText] = useState("1 000 000");
  const [months, setMonths] = useState(12);
  const [keyMode, setKeyMode] = useState<"today" | "custom">("today");
  const [keyText, setKeyText] = useState("");
  const [preset, setPreset] = useState<Preset>("now");
  const [person, setPerson] = useState<string>("");
  const [audiences, setAudiences] = useState<Audience[]>([]);
  const [choices, setChoices] = useState<Record<string, DepositConditionChoice>>({});
  const [open, setOpen] = useState<string | null>(null);

  const amount = useDebounced(parseToMinor(amountText));
  const keyPercent = useDebounced(keyText.replace(",", ".").trim());
  const keyOK = keyMode === "today" || /^\d{1,2}(\.\d{1,3})?$/.test(keyPercent);
  const who = person || session.data?.user.id || "";
  const input: DepositComparisonInput | null = useMemo(
    () =>
      amount !== null && amount > 0 && keyOK
        ? {
            amount_minor: amount,
            months,
            person_id: who || null,
            key_percent: keyMode === "custom" ? keyPercent : null,
            preset,
            choices: Object.values(choices),
            audiences,
          }
        : null,
    [amount, months, keyMode, keyPercent, keyOK, preset, choices, audiences, who],
  );
  const q = useDepositComparison(input);
  const data = q.data;

  const choose = (row: DepositComparisonRow, c: DepositComparisonCondition, value: string) => {
    const k = keyOf(row.programme_id, row.variant_id, c.id);
    setChoices((all) => {
      const next = { ...all };
      if (value === preset) {
        delete next[k];
        return next;
      }
      const [state, level] = value.split(":");
      next[k] = {
        programme_id: row.programme_id,
        variant_id: row.variant_id,
        condition: c.id,
        state: state as Preset,
        level: level === undefined ? null : Number(level),
      };
      return next;
    });
  };

  return (
    <div className="grid gap-6">
      <div className="grid gap-1">
        <h1 className="text-2xl font-bold">{t("deposits.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("deposits.hint")}</p>
      </div>
      <Card>
        <CardContent className="grid gap-4 pt-6 sm:grid-cols-2 lg:grid-cols-4">
          <div className="grid gap-1">
            <Label htmlFor="deposits-amount">{t("deposits.amount")}</Label>
            <Input id="deposits-amount" inputMode="decimal" value={amountText} onChange={(e) => setAmountText(e.target.value)} />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="deposits-months">{t("deposits.horizon")}</Label>
            <Select value={String(months)} onValueChange={(v) => setMonths(Number(v))}>
              <SelectTrigger id="deposits-months">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {HORIZONS.map((m) => (
                  <SelectItem key={m} value={String(m)}>
                    {t("deposits.months", { count: m })}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-1">
            <Label htmlFor="deposits-key">{t("deposits.key")}</Label>
            <div className="flex gap-2">
              <Select value={keyMode} onValueChange={(v) => setKeyMode(v as "today" | "custom")}>
                <SelectTrigger id="deposits-key" className="min-w-0 flex-1">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="today">{t("deposits.keyToday")}</SelectItem>
                  <SelectItem value="custom">{t("deposits.keyCustom")}</SelectItem>
                </SelectContent>
              </Select>
              {keyMode === "custom" && (
                <Input
                  aria-label={t("deposits.keyPercent")}
                  className="w-20"
                  inputMode="decimal"
                  value={keyText}
                  placeholder="12"
                  onChange={(e) => setKeyText(e.target.value)}
                />
              )}
            </div>
          </div>
          <div className="grid gap-1">
            <Label htmlFor="deposits-preset">{t("deposits.preset")}</Label>
            <Select value={preset} onValueChange={(v) => setPreset(v as Preset)}>
              <SelectTrigger id="deposits-preset">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="now">{t("deposits.presetNow")}</SelectItem>
                <SelectItem value="yes">{t("deposits.presetYes")}</SelectItem>
                <SelectItem value="no">{t("deposits.presetNo")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {(members.data?.length ?? 0) > 1 && (
            <div className="grid gap-1">
              <Label htmlFor="deposits-person">{t("deposits.person")}</Label>
              <Select value={who} onValueChange={setPerson}>
                <SelectTrigger id="deposits-person">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {members.data!.map((m) => (
                    <SelectItem key={m.id} value={m.id}>
                      {m.display_name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}
          <fieldset className="grid gap-1.5 sm:col-span-2 lg:col-span-4">
            <legend className="mb-1 text-sm font-medium">{t("deposits.audiences")}</legend>
            <div className="flex flex-wrap gap-x-4 gap-y-1.5">
              {AUDIENCES.map((a) => (
                <label key={a} className="flex items-center gap-2 text-sm">
                  <Checkbox
                    checked={audiences.includes(a)}
                    onCheckedChange={(v) => setAudiences((all) => (v === true ? [...all, a] : all.filter((x) => x !== a)))}
                  />
                  {audienceName(t, a)}
                </label>
              ))}
            </div>
          </fieldset>
        </CardContent>
      </Card>
      {!keyOK && <p className="text-sm text-amber-700 dark:text-amber-400">{t("deposits.keyInvalid")}</p>}
      <QueryGate state={queryState(q)} />
      {data && (
        <Card data-testid="deposits-result" className={cn(q.isFetching && "opacity-70")}>
          <CardHeader className="pb-2">
            <CardTitle className="text-base">
              {t("deposits.resultTitle", { start: formatDate(data.start_on), end: formatDate(data.end_on), count: data.rows.length })}
            </CardTitle>
          </CardHeader>
          <CardContent className="grid gap-3">
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("deposits.columns.product")}</TableHead>
                    <TableHead className="hidden text-right md:table-cell">{t("deposits.columns.income")}</TableHead>
                    <TableHead className="text-right">{t("deposits.columns.yearly")}</TableHead>
                    <TableHead className="hidden text-right md:table-cell">{t("deposits.columns.tax")}</TableHead>
                    <TableHead className="text-right">{t("deposits.columns.net")}</TableHead>
                    <TableHead className="hidden text-right lg:table-cell">{t("deposits.columns.insured")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.rows.map((r) => {
                    const k = `${r.programme_id}|${r.variant_id}`;
                    const isOpen = open === k;
                    const chosen = r.conditions.some((c) => choices[keyOf(r.programme_id, r.variant_id, c.id)]);
                    return (
                      <Fragment key={k}>
                        <TableRow data-testid="deposits-row" className="cursor-pointer" onClick={() => setOpen(isOpen ? null : k)}>
                          <TableCell className="whitespace-normal">
                            <div className="font-medium">
                              {r.bank_name} · {r.programme_name}
                            </div>
                            <div className="text-xs text-muted-foreground">
                              {r.variant_name} · {termOf(t, r)}
                              {r.conditions.length > 0 && ` · ${t("deposits.conditionsCount", { count: r.conditions.filter((c) => c.met).length, of: r.conditions.length })}`}
                              {chosen && ` · ${t("deposits.chosen")}`}
                            </div>
                          </TableCell>
                          <TableCell className="hidden text-right tabular-nums md:table-cell">{formatMinor(r.income_minor, RUB)}</TableCell>
                          <TableCell className="text-right tabular-nums">{percent(r.yearly_percent)}</TableCell>
                          <TableCell className="hidden text-right tabular-nums md:table-cell">
                            {r.tax_minor === null ? "—" : formatMinor(r.tax_minor, RUB)}
                          </TableCell>
                          <TableCell className="text-right font-medium tabular-nums">{formatMinor(r.net_minor, RUB)}</TableCell>
                          <TableCell className="hidden text-right tabular-nums lg:table-cell">
                            {r.insured_minor === null ? "—" : formatMinor(r.insured_minor, RUB)}
                          </TableCell>
                        </TableRow>
                        {isOpen && (
                          <TableRow data-testid="deposits-details">
                            <TableCell colSpan={6} className="whitespace-normal bg-muted/30">
                              <Details row={r} choices={choices} preset={preset} onChoose={choose} />
                            </TableCell>
                          </TableRow>
                        )}
                      </Fragment>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
            <div className="grid gap-1 text-xs text-muted-foreground" data-testid="deposits-notes">
              <p>{t("deposits.noteKey", { key: percent(data.key_percent) })}</p>
              <p>{t("deposits.noteTerm")}</p>
              <p>{t("deposits.noteTax", { person: data.person })}</p>
              <p>{t("deposits.noteInsured", { person: data.person })}</p>
              {data.skipped > 0 && <p>{t("deposits.skipped", { count: data.skipped })}</p>}
            </div>
          </CardContent>
        </Card>
      )}
      {data && data.rows.length === 0 && (
        <Alert>
          <AlertDescription>{t("deposits.empty")}</AlertDescription>
        </Alert>
      )}
    </div>
  );
}

// Details are a row's conditions with their choices, and where its terms come
// from.
function Details({
  row: r,
  choices,
  preset,
  onChoose,
}: {
  row: DepositComparisonRow;
  choices: Record<string, DepositConditionChoice>;
  preset: Preset;
  onChoose: (row: DepositComparisonRow, c: DepositComparisonCondition, value: string) => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="grid gap-3 py-1 text-sm">
      <div className="grid gap-0.5 sm:hidden">
        <div>{t("deposits.detailIncome", { amount: formatMinor(r.income_minor, RUB) })}</div>
        {r.tax_minor !== null && <div>{t("deposits.detailTax", { amount: formatMinor(r.tax_minor, RUB) })}</div>}
        {r.insured_minor !== null && <div>{t("deposits.detailInsured", { amount: formatMinor(r.insured_minor, RUB) })}</div>}
      </div>
      {r.conditions.length === 0 ? (
        <p className="text-muted-foreground">{t("deposits.noConditions")}</p>
      ) : (
        <ul className="grid gap-2" data-testid="deposits-conditions">
          {r.conditions.map((c) => {
            const chosen = choices[keyOf(r.programme_id, r.variant_id, c.id)];
            const value = chosen ? (chosen.state === "yes" && chosen.level != null ? `yes:${chosen.level}` : chosen.state) : preset;
            return (
              <li key={c.id} className="grid gap-1 sm:grid-cols-[1fr_auto] sm:items-center sm:gap-3">
                <div>
                  {c.name}
                  {c.plus !== null && c.levels.length === 0 && <span className="text-muted-foreground"> · +{c.plus}</span>}
                  <div className="text-xs text-muted-foreground">{nowOf(t, c)}</div>
                </div>
                <Select value={value} onValueChange={(v) => onChoose(r, c, v)}>
                  <SelectTrigger className="w-full sm:w-64" aria-label={c.name}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="now">{t("deposits.stateNow")}</SelectItem>
                    {c.levels.length === 0 ? (
                      <SelectItem value="yes">{t("deposits.stateYes")}</SelectItem>
                    ) : (
                      c.levels.map((l, i) => (
                        <SelectItem key={i} value={`yes:${i}`}>
                          {t("deposits.stateLevel", { amount: formatMinor(l.from, RUB), plus: l.plus })}
                        </SelectItem>
                      ))
                    )}
                    <SelectItem value="no">{t("deposits.stateNo")}</SelectItem>
                  </SelectContent>
                </Select>
              </li>
            );
          })}
        </ul>
      )}
      {!r.audience.includes("all") && (
        <p className="text-xs text-muted-foreground">{t("deposits.openTo", { who: r.audience.map((a) => audienceName(t, a)).join(", ") })}</p>
      )}
      <p className="text-xs text-muted-foreground">
        {t("deposits.edition", { date: formatDate(r.edition_from) })}{" "}
        {r.sources.map((s, i) => (
          <a key={s} href={s} target="_blank" rel="noreferrer" className="underline" onClick={(e) => e.stopPropagation()}>
            {t("programme.source", { n: i + 1 })}
          </a>
        ))}
      </p>
    </div>
  );
}

// nowOf says what the family's data tell of the condition today.
function nowOf(t: TFunction, c: DepositComparisonCondition): string {
  if (!c.now_known) return t("deposits.nowUnknown");
  if (c.kind === "requirement") return c.now_met ? t("deposits.nowNewClient") : t("deposits.nowClient");
  if (!c.now_met) return c.measured_minor !== null ? t("deposits.nowNotMet", { amount: formatMinor(c.measured_minor, RUB) }) : t("deposits.nowNo");
  return c.measured_minor !== null ? t("deposits.nowMet", { amount: formatMinor(c.measured_minor, RUB) }) : t("deposits.nowYes");
}

function termOf(t: TFunction, r: DepositComparisonRow): string {
  if (r.kind === "savings") return t("deposits.savings");
  const unit = r.term_unit === "months" ? t("deposits.monthsShort") : t("deposits.daysShort");
  const pays = r.payout === "capitalize" ? t("deposits.paysCapitalize") : r.payout === "monthly" ? t("deposits.paysMonthly") : t("deposits.paysEnd");
  return r.prolonged > 0
    ? t("deposits.termProlonged", { term: r.term, unit, times: r.prolonged + 1, pays })
    : t("deposits.term", { term: r.term, unit, pays });
}

function audienceName(t: TFunction, a: string): string {
  switch (a) {
    case "pensioner":
      return t("deposits.audience.pensioner");
    case "youth":
      return t("deposits.audience.youth");
    case "social":
      return t("deposits.audience.social");
    case "salary":
      return t("deposits.audience.salary");
    case "premium":
      return t("deposits.audience.premium");
    case "bundle":
      return t("deposits.audience.bundle");
    case "new":
      return t("deposits.audience.new");
    case "regional":
      return t("deposits.audience.regional");
    default:
      return a;
  }
}

function percent(s: string): string {
  return `${Number(s).toLocaleString("ru-RU", { maximumFractionDigits: 2 })} %`;
}
