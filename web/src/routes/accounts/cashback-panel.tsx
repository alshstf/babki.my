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
  useCardCashback,
  useDeleteCardLoyalty,
  useLoyaltyProgrammes,
  useSetCardLoyalty,
  useSetCardPicks,
  type CardCashback,
} from "@/api/cashback";
import { formatDate } from "@/lib/dates";
import { formatMinor } from "@/lib/money";
import { rate } from "./programme-section";

// shiftDay is the ISO day n days from iso.
function shiftDay(iso: string, n: number): string {
  const d = new Date(`${iso}T00:00:00Z`);
  d.setUTCDate(d.getUTCDate() + n);
  return d.toISOString().slice(0, 10);
}

// CashbackPanel is a card's bank loyalty programme (decision Р-58): the
// programme it is tied to, the categories the family picked for the period
// with the bank's percents, and what the period's purchases earn by their
// merchant codes — an estimate.
export function CashbackPanel({ account, canEdit }: { account: AccountWithBalance; canEdit: boolean }) {
  const { t } = useTranslation();
  const [on, setOn] = useState<string | null>(null);
  const [picking, setPicking] = useState(false);
  const q = useCardCashback(account.id, on);
  const unbind = useDeleteCardLoyalty(account.id);
  if (q.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{t("app.error")}</AlertDescription>
      </Alert>
    );
  }
  const data = q.data;
  if (!data) return null;
  const p = data.programme;
  if (!p) return <BindForm account={account} canEdit={canEdit} />;
  const from = data.period_from;
  const to = data.period_to;
  return (
    <div className="grid gap-3 text-sm" data-testid="cashback">
      <div className="flex flex-wrap items-center gap-2">
        <span>{t("cashback.tied", { bank: p.bank_name, programme: p.programme_name, variant: p.variant_name })}</span>
        {canEdit && (
          <Button size="sm" variant="ghost" disabled={unbind.isPending} onClick={() => unbind.mutate()}>
            {t("cashback.untie")}
          </Button>
        )}
      </div>
      {!p.known || from === null || to === null ? (
        <p className="text-muted-foreground">{t("cashback.unknown")}</p>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" variant="ghost" aria-label={t("cashback.previous")} onClick={() => setOn(shiftDay(from, -1))}>
              ←
            </Button>
            <span className="font-medium" data-testid="cashback-period">
              {t("cashback.period", { from: formatDate(from), to: formatDate(to) })}
            </span>
            <Button size="sm" variant="ghost" aria-label={t("cashback.next")} onClick={() => setOn(shiftDay(to, 1))}>
              →
            </Button>
          </div>
          {p.period !== "none" && (
            <p className="text-xs text-muted-foreground">
              {p.of > 0 ? t("cashback.picksOf", { pick: p.pick, of: p.of }) : t("cashback.picks", { pick: p.pick })}
            </p>
          )}
          {data.lines.length === 0 ? (
            <p className="text-amber-700 dark:text-amber-400" data-testid="cashback-none">{t("cashback.nonePicked")}</p>
          ) : (
            <ul className="grid gap-1" data-testid="cashback-lines">
              {data.lines.map((l) => (
                <li key={l.category_id} className="flex flex-wrap justify-between gap-2">
                  <span>{t("cashback.line", { name: l.name, percent: rate(l.percent), spent: formatMinor(l.spent_minor, "RUB") })}</span>
                  <span className="tabular-nums">{formatMinor(l.bonus_minor, "RUB")}</span>
                </li>
              ))}
            </ul>
          )}
          {data.lines.length > 0 && (
            <p className="font-medium" data-testid="cashback-total">
              {t("cashback.total", { amount: formatMinor(data.bonus_minor, "RUB") })}
              {data.capped && ` ${t("cashback.capped", { cap: formatMinor(p.cap_minor, "RUB") })}`}
            </p>
          )}
          {data.uncoded_count > 0 && (
            <p className="text-xs text-muted-foreground">
              {t("cashback.uncoded", { count: data.uncoded_count, amount: formatMinor(data.uncoded_minor, "RUB") })}
            </p>
          )}
          {canEdit && p.period !== "none" && (
            <Button size="sm" variant="outline" className="justify-self-start" onClick={() => setPicking(true)}>
              {data.lines.length === 0 ? t("cashback.pick") : t("cashback.changePicks")}
            </Button>
          )}
          <p className="text-xs text-muted-foreground">{t("cashback.hint")}</p>
        </>
      )}
      {picking && from !== null && <PicksDialog account={account} data={data} on={from} onClose={() => setPicking(false)} />}
    </div>
  );
}

// BindForm ties the card to a programme of the catalog and its variant.
function BindForm({ account, canEdit }: { account: AccountWithBalance; canEdit: boolean }) {
  const { t } = useTranslation();
  const list = useLoyaltyProgrammes(canEdit);
  const bind = useSetCardLoyalty(account.id);
  const [programme, setProgramme] = useState("");
  const [variant, setVariant] = useState("");
  const chosen = list.data?.find((p) => p.id === programme);
  return (
    <div className="grid gap-2 text-sm" data-testid="cashback">
      <p className="text-muted-foreground">{t("cashback.notTied")}</p>
      {canEdit && list.data && (
        <div className="grid gap-2">
          <Select
            value={programme}
            onValueChange={(v) => {
              setProgramme(v);
              setVariant("");
            }}
          >
            <SelectTrigger aria-label={t("cashback.programme")}>
              <SelectValue placeholder={t("cashback.programme")} />
            </SelectTrigger>
            <SelectContent>
              {list.data.map((p) => (
                <SelectItem key={p.id} value={p.id}>
                  {`${p.bank_name} · ${p.name}`}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {chosen && (
            <Select value={variant} onValueChange={setVariant}>
              <SelectTrigger aria-label={t("cashback.variant")}>
                <SelectValue placeholder={t("cashback.variant")} />
              </SelectTrigger>
              <SelectContent>
                {chosen.variants.map((v) => (
                  <SelectItem key={v.id} value={v.id}>
                    {v.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          <Button
            size="sm"
            className="justify-self-start"
            disabled={!programme || !variant || bind.isPending}
            onClick={() => bind.mutate({ programme_id: programme, variant_id: variant })}
          >
            {t("cashback.tie")}
          </Button>
          {bind.isError && <p className="text-xs text-red-700 dark:text-red-400">{t("cashback.failed")}</p>}
        </div>
      )}
    </div>
  );
}

// PicksDialog says which categories the family picked for the period in the
// bank's app, at the percents the bank offered: no more than the variant
// picks and one more.
function PicksDialog({ account, data, on, onClose }: { account: AccountWithBalance; data: CardCashback; on: string; onClose: () => void }) {
  const { t } = useTranslation();
  const save = useSetCardPicks(account.id);
  const p = data.programme!;
  const [chosen, setChosen] = useState<Record<string, string>>(() =>
    Object.fromEntries(data.lines.map((l) => [l.category_id, l.percent.replace(".", ",")])),
  );
  const limit = p.pick + 1;
  const ids = Object.keys(chosen);
  const percent = (s: string) => s.trim().replace(",", ".");
  const valid = ids.every((id) => /^\d{1,3}(\.\d{1,2})?$/.test(percent(chosen[id])) && Number(percent(chosen[id])) > 0 && Number(percent(chosen[id])) <= 100);
  const submit = () =>
    save.mutate({ on, picks: ids.map((id) => ({ category_id: id, percent: percent(chosen[id]) })) }, { onSuccess: onClose });
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-md" data-testid="cashback-picks">
        <DialogHeader>
          <DialogTitle>{t("cashback.picksTitle", { from: formatDate(data.period_from ?? on), to: formatDate(data.period_to ?? on) })}</DialogTitle>
        </DialogHeader>
        <p className="text-xs text-muted-foreground">{t("cashback.picksHint", { pick: p.pick })}</p>
        <div className="grid max-h-80 gap-1 overflow-y-auto pr-1">
          {p.categories.map((c) => {
            const on = c.id in chosen;
            return (
              <div key={c.id} className="flex items-center justify-between gap-2">
                <div className="flex items-center gap-2">
                  <Checkbox
                    id={`pick-${c.id}`}
                    checked={on}
                    disabled={!on && ids.length >= limit}
                    onCheckedChange={(v) =>
                      setChosen((prev) => {
                        const next = { ...prev };
                        if (v === true) next[c.id] = "";
                        else delete next[c.id];
                        return next;
                      })
                    }
                  />
                  <Label htmlFor={`pick-${c.id}`} className="font-normal">
                    {c.name}
                    {c.by_merchant && <span className="text-xs text-muted-foreground"> · {t("cashback.byMerchant")}</span>}
                  </Label>
                </div>
                {on && (
                  <Input
                    aria-label={t("cashback.percentOf", { name: c.name })}
                    inputMode="decimal"
                    className="h-8 w-20"
                    placeholder="5"
                    value={chosen[c.id]}
                    onChange={(e) => setChosen((prev) => ({ ...prev, [c.id]: e.target.value }))}
                  />
                )}
              </div>
            );
          })}
        </div>
        {save.isError && (
          <Alert variant="destructive">
            <AlertDescription>{t("cashback.picksFailed")}</AlertDescription>
          </Alert>
        )}
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
