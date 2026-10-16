import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Trash2 } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import type { AccountWithBalance } from "@/api/accounts";
import { useAccountRates, useDeleteAccountRate, useSetAccountRate, type AccountRate } from "@/api/account-rates";
import { formatDate, localToday } from "@/lib/dates";
import { ProgrammeBlock, QuoteLine, rate } from "./programme-section";

// A key-tied rate's margin with its sign: "+ 1 %", "− 1,5 %".
function margin(s: string): string {
  const n = Number(s);
  return `${n < 0 ? "−" : "+"} ${rate(String(Math.abs(n)))}`;
}

// The row as words: "16 % годовых" or "ключевая − 1,5 %".
function terms(t: (key: string, o?: Record<string, string>) => string, r: AccountRate): string {
  return r.kind === "key" ? t("rates.keyTerms", { margin: margin(r.percent) }) : t("rates.fixedTerms", { percent: rate(r.percent) });
}

// RatePanel is a savings account's or a deposit's rate: the one in force
// today and where it comes from — the bank's terms of the programme the
// account is tied to (Р-53 Б), or one written in by hand, which stands over
// them (Р-51 А); then the programme with its conditions and the rates by
// hand. A goal on the account grows at it.
export function RatePanel({ account, canEdit }: { account: AccountWithBalance; canEdit: boolean }) {
  const { t } = useTranslation();
  const q = useAccountRates(account.id);
  const remove = useDeleteAccountRate(account.id);
  const [adding, setAdding] = useState(false);
  if (q.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{t("app.error")}</AlertDescription>
      </Alert>
    );
  }
  const data = q.data;
  if (!data) return null;
  const now = data.source === "by_hand" ? data.current : null;
  const quote = data.quote;
  // A key-tied rate in force, but no key rate to strike it with yet.
  const waitsForKey = !now && !quote && data.rates.some((r) => r.from_on <= localToday());
  return (
    <div className="grid gap-4" data-testid="account-rate">
      {now ? (
        <div className="grid gap-0.5">
          <div className="text-2xl font-bold tabular-nums" data-testid="account-rate-now">
            {t("rates.now", { percent: rate(now.percent) })}
          </div>
          <div className="text-sm text-muted-foreground">
            {now.kind === "key" && data.key_rate !== null && `${t("rates.byKey", { key: rate(data.key_rate), margin: margin(data.rates.find((r) => r.from_on === now.from_on)?.percent ?? "0") })} · `}
            {t("rates.since", { date: formatDate(now.from_on) })}
            {now.by_hand && ` · ${t("rates.byHand")}`}
          </div>
          {quote && <div className="text-sm text-muted-foreground">{t("programme.wouldBe", { percent: rate(quote.percent) })}</div>}
        </div>
      ) : quote ? (
        <div className="grid gap-0.5">
          <div className="text-2xl font-bold tabular-nums" data-testid="account-rate-now">
            {t("rates.now", { percent: rate(quote.percent) })}
          </div>
          <QuoteLine data={data} currency={account.currency} />
        </div>
      ) : waitsForKey ? (
        <p className="text-sm text-amber-700 dark:text-amber-400">{t("rates.noKey")}</p>
      ) : (
        <p className="text-sm text-muted-foreground">{t("rates.none")}</p>
      )}

      <div className="grid gap-2">
        <h3 className="text-sm font-semibold">{t("programme.title")}</h3>
        <ProgrammeBlock account={account} data={data} canEdit={canEdit} />
      </div>

      <div className="grid gap-2">
        <h3 className="text-sm font-semibold">{t("rates.manualTitle")}</h3>
      {data.rates.length > 0 && (
        <ul className="grid gap-1 text-sm" data-testid="account-rate-history">
          {[...data.rates].reverse().map((r) => (
            <li key={r.from_on} className="flex items-center justify-between gap-2">
              <span>
                <span className="tabular-nums text-muted-foreground">{t("rates.from", { date: formatDate(r.from_on) })}</span> — {terms(t, r)}
                {r.by_hand && <span className="text-muted-foreground"> · {t("rates.byHandShort")}</span>}
              </span>
              {canEdit && (
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-7"
                  aria-label={t("rates.remove")}
                  disabled={remove.isPending}
                  onClick={() => remove.mutate(r.from_on)}
                >
                  <Trash2 className="size-4" />
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}

      {canEdit && (
        <div>
          <Button variant="outline" size="sm" onClick={() => setAdding(true)}>
            {data.rates.length > 0 ? t("rates.change") : t("rates.add")}
          </Button>
        </div>
      )}
      <p className="text-xs text-muted-foreground">{t("rates.hint")}</p>
      </div>
      {adding && <RateDialog account={account} keyRate={data.key_rate} onClose={() => setAdding(false)} />}
    </div>
  );
}

// RateDialog states a rate from a day: fixed, or the key rate plus a margin.
function RateDialog({ account, keyRate, onClose }: { account: AccountWithBalance; keyRate: string | null; onClose: () => void }) {
  const { t } = useTranslation();
  const save = useSetAccountRate(account.id);
  const [fromOn, setFromOn] = useState(localToday);
  const [kind, setKind] = useState<"fixed" | "key">("fixed");
  const [text, setText] = useState("");
  // Typed with a comma or a point, a minus only for a margin, to thousandths.
  const value = text.trim().replace(/\s/g, "").replace(",", ".").replace("−", "-");
  const ok = (kind === "fixed" ? /^\d{1,2}(\.\d{1,3})?$/ : /^[-+]?\d{1,2}(\.\d{1,3})?$/).test(value);
  const valid = /^\d{4}-\d{2}-\d{2}$/.test(fromOn) && ok;
  const submit = () => {
    if (!valid) return;
    save.mutate({ from_on: fromOn, kind, percent: value.replace("+", "") }, { onSuccess: onClose });
  };
  const example = keyRate !== null && ok && kind === "key" ? Math.max(Number(keyRate) + Number(value), 0) : null;
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-md" data-testid="account-rate-dialog">
        <DialogHeader>
          <DialogTitle>{t("rates.dialogTitle", { name: account.name })}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1">
            <Label htmlFor="rate-kind">{t("rates.kind")}</Label>
            <Select value={kind} onValueChange={(v) => setKind(v as "fixed" | "key")}>
              <SelectTrigger id="rate-kind">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="fixed">{t("rates.kinds.fixed")}</SelectItem>
                <SelectItem value="key">{t("rates.kinds.key")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="grid grid-cols-2 gap-2">
            <div className="grid gap-1">
              <Label htmlFor="rate-percent">{kind === "fixed" ? t("rates.percent") : t("rates.margin")}</Label>
              <Input
                id="rate-percent"
                inputMode="decimal"
                value={text}
                placeholder={kind === "fixed" ? "16" : "-1,5"}
                onChange={(e) => setText(e.target.value)}
              />
            </div>
            <div className="grid gap-1">
              <Label htmlFor="rate-from">{t("rates.fromOn")}</Label>
              <Input id="rate-from" type="date" value={fromOn} onChange={(e) => setFromOn(e.target.value)} />
            </div>
          </div>
          <p className="text-xs text-muted-foreground">
            {kind === "key"
              ? example !== null && keyRate !== null
                ? t("rates.keyExample", { key: rate(keyRate), percent: rate(String(example)) })
                : t("rates.keyHint")
              : t("rates.fixedHint")}
          </p>
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("rates.failed")}</AlertDescription>
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
