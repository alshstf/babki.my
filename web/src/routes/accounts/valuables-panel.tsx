import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Pencil, Trash2 } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { AccountWithBalance } from "@/api/accounts";
import { useMembers } from "@/api/members";
import {
  useDeleteItem,
  useSaveItem,
  useSetBankPrice,
  useSetStorage,
  useValuables,
  type Metal,
  type Valuables,
  type ValuablesItem,
  type ValuablesItemInput,
  type ValuablesStorage,
} from "@/api/valuables";
import { formatMinor, minorToInput, parseToMinor } from "@/lib/money";
import { formatDate, localToday } from "@/lib/dates";
import { cn } from "@/lib/utils";

const METALS: Metal[] = ["gold", "silver", "platinum", "palladium"];
const RUB = "RUB";

// A decimal as the API writes it ("999.9"), as people read it ("999,9").
function num(s: string): string {
  return Number(s).toLocaleString("ru-RU", { maximumFractionDigits: 4 });
}

// A decimal typed with a comma or a point, as the API takes it; null when it
// is not one.
function decimalOf(s: string): string | null {
  const v = s.trim().replace(/\s/g, "").replace(",", ".");
  return /^\d+(\.\d+)?$/.test(v) ? v : null;
}

// ValuablesPanel is a valuables account (decisions Р-39, Р-40): where its
// bars and coins are kept, each piece with what it would sell for now and
// is worth by the central bank's price, from when it sells free of tax and
// why a bank might not take it back without an assay.
export function ValuablesPanel({ account, canEdit }: { account: AccountWithBalance; canEdit: boolean }) {
  const { t } = useTranslation();
  const q = useValuables(account.id);
  const [storageOpen, setStorageOpen] = useState(false);
  const [item, setItem] = useState<ValuablesItem | "new" | null>(null);
  const [bankOpen, setBankOpen] = useState(false);
  const remove = useDeleteItem(account.id);
  const data = q.data;
  if (q.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{t("app.error")}</AlertDescription>
      </Alert>
    );
  }
  if (!data) return null;
  const today = localToday();
  const byGuide = data.items.some((it) => it.sell_by === "guide_percent");
  return (
    <div className="grid gap-4" data-testid="valuables">
      <div className="grid gap-1">
        {data.sell_minor !== null ? (
          <>
            <div className="text-2xl font-bold tabular-nums" data-testid="valuables-sell">
              {formatMinor(data.sell_minor, RUB)}
            </div>
            <div className="text-sm text-muted-foreground">
              {t("valuables.sellNow")}
              {data.market_minor !== null && ` · ${t("valuables.byCbr", { amount: formatMinor(data.market_minor, RUB) })}`}
            </div>
          </>
        ) : (
          <p className="text-sm text-amber-700 dark:text-amber-400">{t("valuables.notAllPriced")}</p>
        )}
      </div>

      <StorageLine storage={data.storage} today={today} canEdit={canEdit} onEdit={() => setStorageOpen(true)} />

      {data.items.length === 0 ? (
        <div className="rounded-lg border border-dashed p-8 text-center text-sm text-muted-foreground">{t("valuables.empty")}</div>
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("valuables.columns.piece")}</TableHead>
                <TableHead className="hidden sm:table-cell">{t("valuables.columns.bought")}</TableHead>
                <TableHead className="text-right">{t("valuables.columns.sell")}</TableHead>
                <TableHead className="hidden text-right md:table-cell">{t("valuables.columns.market")}</TableHead>
                <TableHead className="hidden sm:table-cell">{t("valuables.columns.tax")}</TableHead>
                {canEdit && <TableHead />}
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.items.map((it) => (
                <TableRow key={it.id} data-testid="valuables-item">
                  <TableCell className="whitespace-normal">
                    <div>
                      {t(`valuables.kinds.${it.kind}`)}, {t(`valuables.metals.${it.metal}`)}, {num(it.weight)} {t("valuables.grams")}, {num(it.fineness)}
                    </div>
                    <div className="text-xs text-muted-foreground">
                      {[it.maker, it.serial && t("valuables.serial", { serial: it.serial })].filter(Boolean).join(" · ")}
                    </div>
                    {/* On a phone the tax goes under the piece, not in a column. */}
                    <div className={cn("mt-1 text-xs sm:hidden", taxClass(it, today))}>{taxText(t, it, today)}</div>
                    {it.hard_to_sell.length > 0 && (
                      <div className="mt-1 flex flex-wrap gap-1" title={t("valuables.hardHint")}>
                        {it.hard_to_sell.map((h) => (
                          <Badge key={h} variant="outline" className="border-amber-600/60 text-amber-700 dark:text-amber-400">
                            {t(`valuables.hard.${h}`)}
                          </Badge>
                        ))}
                      </div>
                    )}
                  </TableCell>
                  <TableCell className="hidden whitespace-nowrap sm:table-cell">
                    {formatDate(it.bought_on)}
                    <div className="text-xs text-muted-foreground">{formatMinor(it.cost_minor, it.currency)}</div>
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {it.sell_minor !== null ? formatMinor(it.sell_minor, RUB) : "—"}
                    <div className="min-w-28 whitespace-normal text-xs text-muted-foreground">{sellNote(t, it)}</div>
                  </TableCell>
                  <TableCell className="hidden text-right tabular-nums md:table-cell">
                    {it.market_minor !== null ? formatMinor(it.market_minor, RUB) : "—"}
                  </TableCell>
                  <TableCell className={cn("hidden whitespace-normal text-xs sm:table-cell", taxClass(it, today))}>{taxText(t, it, today)}</TableCell>
                  {canEdit && (
                    <TableCell className="whitespace-nowrap text-right">
                      <Button variant="ghost" size="icon" aria-label={t("valuables.edit")} onClick={() => setItem(it)}>
                        <Pencil className="size-4" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={t("valuables.delete")}
                        onClick={() => {
                          if (window.confirm(t("valuables.deleteConfirm"))) remove.mutate(it.id);
                        }}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      {byGuide && <p className="text-xs text-muted-foreground">{t("valuables.guideHint")}</p>}
      {data.bank_prices.length > 0 && (
        <p className="text-xs text-muted-foreground" data-testid="valuables-bank-prices">
          {data.bank_prices
            .map((b) => t("valuables.bankPrice", { metal: t(`valuables.metals.${b.metal}`), price: num(b.price), date: formatDate(b.on) }))
            .join(" · ")}
        </p>
      )}
      {canEdit && (
        <div className="flex flex-wrap gap-2">
          <Button size="sm" onClick={() => setItem("new")}>
            {t("valuables.add")}
          </Button>
          <Button size="sm" variant="outline" onClick={() => setBankOpen(true)}>
            {t("valuables.setBankPrice")}
          </Button>
        </div>
      )}
      {storageOpen && <StorageDialog accountId={account.id} storage={data.storage} onClose={() => setStorageOpen(false)} />}
      {item && <ItemDialog accountId={account.id} item={item === "new" ? null : item} onClose={() => setItem(null)} />}
      {bankOpen && <BankPriceDialog accountId={account.id} data={data} onClose={() => setBankOpen(false)} />}
    </div>
  );
}

// taxText says from when a piece sells free of tax.
function taxText(t: (key: string, v?: Record<string, unknown>) => string, it: ValuablesItem, today: string): string {
  return it.tax_free_from <= today ? t("valuables.taxFree") : t("valuables.taxFreeFrom", { date: formatDate(it.tax_free_from) });
}

function taxClass(it: ValuablesItem, today: string): string {
  return it.tax_free_from <= today ? "text-emerald-700 dark:text-emerald-400" : "text-muted-foreground";
}

// sellNote says where «if sold now» comes from.
function sellNote(t: (key: string, v?: Record<string, unknown>) => string, it: ValuablesItem): string {
  switch (it.sell_by) {
    case "bank_price":
      return t("valuables.by.bankPrice", { date: formatDate(it.bank_price_on ?? "") });
    case "storage_percent":
      return t("valuables.by.storagePercent", { percent: num(it.buyback_percent ?? "0") });
    case "guide_percent":
      return t("valuables.by.guidePercent", { percent: num(it.buyback_percent ?? "0") });
    default:
      return t("valuables.by.noPrice");
  }
}

// StorageLine is where the pieces are kept and on what terms; a box's rent
// ending within a month, or over, said so.
function StorageLine({ storage, today, canEdit, onEdit }: { storage: ValuablesStorage; today: string; canEdit: boolean; onEdit: () => void }) {
  const { t } = useTranslation();
  const parts = [t(`valuables.places.${storage.place}`)];
  if (storage.bank) parts.push(storage.bank);
  if (storage.until) parts.push(t("valuables.until", { date: formatDate(storage.until) }));
  if (storage.fee_minor !== null && storage.fee_period) {
    parts.push(
      storage.fee_period === "month"
        ? t("valuables.feeMonth", { amount: formatMinor(storage.fee_minor, RUB) })
        : t("valuables.feeYear", { amount: formatMinor(storage.fee_minor, RUB) }),
    );
  }
  const soon = new Date(`${today}T00:00:00`);
  soon.setMonth(soon.getMonth() + 1);
  const ending = storage.until !== null && storage.until <= soon.toISOString().slice(0, 10);
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm" data-testid="valuables-storage">
      <span>{parts.join(" · ")}</span>
      {storage.until !== null && storage.until < today && (
        <span className="text-red-700 dark:text-red-400">{t("valuables.ended")}</span>
      )}
      {ending && storage.until !== null && storage.until >= today && (
        <span className="text-amber-700 dark:text-amber-400">{t("valuables.endingSoon")}</span>
      )}
      {canEdit && (
        <Button variant="outline" size="sm" onClick={onEdit}>
          {t("valuables.editStorage")}
        </Button>
      )}
    </div>
  );
}

function StorageDialog({ accountId, storage, onClose }: { accountId: string; storage: ValuablesStorage; onClose: () => void }) {
  const { t } = useTranslation();
  const save = useSetStorage(accountId);
  const [place, setPlace] = useState(storage.place);
  const [bank, setBank] = useState(storage.bank);
  const [until, setUntil] = useState(storage.until ?? "");
  const [fee, setFee] = useState(storage.fee_minor !== null ? minorToInput(storage.fee_minor) : "");
  const [period, setPeriod] = useState<"month" | "year">(storage.fee_period ?? "month");
  const [buyback, setBuyback] = useState(storage.buyback_percent?.replace(/\.00$/, "") ?? "");
  const feeMinor = fee.trim() === "" ? null : parseToMinor(fee);
  const buybackValue = buyback.trim() === "" ? null : decimalOf(buyback);
  const banked = place === "safe_box" || place === "custody";
  const valid = (fee.trim() === "" || (feeMinor !== null && feeMinor >= 0)) && (buyback.trim() === "" || (buybackValue !== null && Number(buybackValue) < 100));
  const submit = () =>
    save.mutate(
      {
        place,
        bank: banked ? bank : "",
        until: banked && until ? until : null,
        fee_minor: banked ? feeMinor : null,
        fee_period: banked && feeMinor !== null ? period : null,
        buyback_percent: buybackValue,
      },
      { onSuccess: onClose },
    );
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("valuables.storageTitle")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1">
            <Label htmlFor="valuables-place">{t("valuables.place")}</Label>
            <Select value={place} onValueChange={(v) => setPlace(v as ValuablesStorage["place"])}>
              <SelectTrigger id="valuables-place">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="home">{t("valuables.places.home")}</SelectItem>
                <SelectItem value="safe_box">{t("valuables.places.safe_box")}</SelectItem>
                <SelectItem value="custody">{t("valuables.places.custody")}</SelectItem>
                <SelectItem value="other">{t("valuables.places.other")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {banked && (
            <>
              <div className="grid gap-1">
                <Label htmlFor="valuables-bank">{t("valuables.bank")}</Label>
                <Input id="valuables-bank" value={bank} onChange={(e) => setBank(e.target.value)} />
              </div>
              <div className="grid gap-1">
                <Label htmlFor="valuables-until">{t("valuables.untilLabel")}</Label>
                <Input id="valuables-until" type="date" value={until} onChange={(e) => setUntil(e.target.value)} />
              </div>
              <div className="flex flex-wrap items-end gap-2">
                <div className="grid gap-1">
                  <Label htmlFor="valuables-fee">{t("valuables.fee")}</Label>
                  <Input id="valuables-fee" inputMode="decimal" className="w-36" value={fee} onChange={(e) => setFee(e.target.value)} />
                </div>
                <Select value={period} onValueChange={(v) => setPeriod(v === "year" ? "year" : "month")}>
                  <SelectTrigger className="w-32" aria-label={t("valuables.feePeriod")}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="month">{t("valuables.perMonth")}</SelectItem>
                    <SelectItem value="year">{t("valuables.perYear")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </>
          )}
          <div className="grid gap-1">
            <Label htmlFor="valuables-buyback">{t("valuables.buyback")}</Label>
            <Input id="valuables-buyback" inputMode="decimal" className="w-36" value={buyback} onChange={(e) => setBuyback(e.target.value)} />
            <p className="text-xs text-muted-foreground">{t("valuables.buybackHint")}</p>
          </div>
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("valuables.saveFailed")}</AlertDescription>
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

function ItemDialog({ accountId, item, onClose }: { accountId: string; item: ValuablesItem | null; onClose: () => void }) {
  const { t } = useTranslation();
  const save = useSaveItem(accountId);
  const members = useMembers();
  const [kind, setKind] = useState<ValuablesItemInput["kind"]>(item?.kind ?? "bar");
  const [metal, setMetal] = useState<Metal>(item?.metal ?? "gold");
  const [weight, setWeight] = useState(item ? num(item.weight) : "");
  const [fineness, setFineness] = useState(item ? num(item.fineness) : "999,9");
  const [maker, setMaker] = useState(item?.maker ?? "");
  const [foreign, setForeign] = useState(item?.foreign_made ?? false);
  const [serial, setSerial] = useState(item?.serial ?? "");
  const [certificate, setCertificate] = useState(item?.certificate ?? true);
  const [sealed, setSealed] = useState(item?.sealed ?? true);
  const [bought, setBought] = useState(item?.bought_on ?? localToday());
  const [cost, setCost] = useState(item ? minorToInput(item.cost_minor) : "");
  const [owner, setOwner] = useState(item?.owner_user_id ?? "family");
  const [note, setNote] = useState(item?.note ?? "");
  useEffect(() => save.reset(), [kind, metal, weight, fineness, bought, cost]); // eslint-disable-line react-hooks/exhaustive-deps
  const weightValue = decimalOf(weight);
  const finenessValue = decimalOf(fineness);
  const costMinor = parseToMinor(cost);
  const valid =
    weightValue !== null && Number(weightValue) > 0 && finenessValue !== null && Number(finenessValue) > 0 && Number(finenessValue) <= 1000 &&
    costMinor !== null && costMinor >= 0 && bought !== "" && bought <= localToday();
  const submit = () => {
    if (!valid || weightValue === null || finenessValue === null || costMinor === null) return;
    save.mutate(
      {
        id: item?.id,
        body: {
          kind, metal, weight: weightValue, fineness: finenessValue, maker, foreign_made: foreign, serial, certificate, sealed,
          bought_on: bought, cost_minor: costMinor, currency: RUB, owner_user_id: owner === "family" ? null : owner, note,
        },
      },
      { onSuccess: onClose },
    );
  };
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{item ? t("valuables.editTitle") : t("valuables.addTitle")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="flex flex-wrap gap-2">
            <Select value={kind} onValueChange={(v) => setKind(v === "coin" ? "coin" : "bar")}>
              <SelectTrigger className="w-36" aria-label={t("valuables.kind")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="bar">{t("valuables.kinds.bar")}</SelectItem>
                <SelectItem value="coin">{t("valuables.kinds.coin")}</SelectItem>
              </SelectContent>
            </Select>
            <Select value={metal} onValueChange={(v) => setMetal(v as Metal)}>
              <SelectTrigger className="w-36" aria-label={t("valuables.metal")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {METALS.map((m) => (
                  <SelectItem key={m} value={m}>
                    {t(`valuables.metals.${m}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="flex flex-wrap gap-2">
            <div className="grid gap-1">
              <Label htmlFor="valuables-weight">{t("valuables.weight")}</Label>
              <Input id="valuables-weight" inputMode="decimal" className="w-32" value={weight} onChange={(e) => setWeight(e.target.value)} />
            </div>
            <div className="grid gap-1">
              <Label htmlFor="valuables-fineness">{t("valuables.fineness")}</Label>
              <Input id="valuables-fineness" inputMode="decimal" className="w-32" value={fineness} onChange={(e) => setFineness(e.target.value)} />
            </div>
          </div>
          <div className="flex flex-wrap gap-2">
            <div className="grid gap-1">
              <Label htmlFor="valuables-maker">{t("valuables.maker")}</Label>
              <Input id="valuables-maker" className="w-56" value={maker} onChange={(e) => setMaker(e.target.value)} />
            </div>
            <div className="grid gap-1">
              <Label htmlFor="valuables-serial">{t("valuables.serialLabel")}</Label>
              <Input id="valuables-serial" className="w-40" value={serial} onChange={(e) => setSerial(e.target.value)} />
            </div>
          </div>
          <div className="grid gap-2">
            <Label className="flex items-center gap-2 font-normal">
              <Checkbox checked={foreign} onCheckedChange={(v) => setForeign(v === true)} />
              {t("valuables.foreignMade")}
            </Label>
            <Label className="flex items-center gap-2 font-normal">
              <Checkbox checked={certificate} onCheckedChange={(v) => setCertificate(v === true)} />
              {t("valuables.certificate")}
            </Label>
            <Label className="flex items-center gap-2 font-normal">
              <Checkbox checked={sealed} onCheckedChange={(v) => setSealed(v === true)} />
              {t("valuables.sealed")}
            </Label>
          </div>
          <div className="flex flex-wrap gap-2">
            <div className="grid gap-1">
              <Label htmlFor="valuables-bought">{t("valuables.boughtOn")}</Label>
              <Input id="valuables-bought" type="date" className="w-40" value={bought} max={localToday()} onChange={(e) => setBought(e.target.value)} />
            </div>
            <div className="grid gap-1">
              <Label htmlFor="valuables-cost">{t("valuables.cost")}</Label>
              <Input id="valuables-cost" inputMode="decimal" className="w-40" value={cost} onChange={(e) => setCost(e.target.value)} />
            </div>
          </div>
          <div className="grid gap-1">
            <Label htmlFor="valuables-owner">{t("valuables.owner")}</Label>
            <Select value={owner} onValueChange={setOwner}>
              <SelectTrigger id="valuables-owner" className="w-56">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="family">{t("valuables.ownerFamily")}</SelectItem>
                {(members.data ?? []).map((m) => (
                  <SelectItem key={m.id} value={m.id}>
                    {m.display_name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-1">
            <Label htmlFor="valuables-note">{t("valuables.note")}</Label>
            <Input id="valuables-note" value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("valuables.saveFailed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || save.isPending} onClick={submit}>
            {item ? t("common.save") : t("valuables.addButton")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function BankPriceDialog({ accountId, data, onClose }: { accountId: string; data: Valuables; onClose: () => void }) {
  const { t } = useTranslation();
  const save = useSetBankPrice(accountId);
  const held = METALS.filter((m) => data.items.some((it) => it.metal === m));
  const [metal, setMetal] = useState<Metal>(held[0] ?? "gold");
  const [price, setPrice] = useState("");
  const [on, setOn] = useState(localToday());
  const value = decimalOf(price);
  const valid = value !== null && Number(value) > 0 && on !== "" && on <= localToday();
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("valuables.bankTitle")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <p className="text-xs text-muted-foreground">{t("valuables.bankHint")}</p>
          <Select value={metal} onValueChange={(v) => setMetal(v as Metal)}>
            <SelectTrigger className="w-40" aria-label={t("valuables.metal")}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {METALS.map((m) => (
                <SelectItem key={m} value={m}>
                  {t(`valuables.metals.${m}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <div className="flex flex-wrap gap-2">
            <div className="grid gap-1">
              <Label htmlFor="valuables-bank-price">{t("valuables.pricePerGram")}</Label>
              <Input id="valuables-bank-price" inputMode="decimal" className="w-36" value={price} onChange={(e) => setPrice(e.target.value)} />
            </div>
            <div className="grid gap-1">
              <Label htmlFor="valuables-bank-on">{t("valuables.priceOn")}</Label>
              <Input id="valuables-bank-on" type="date" className="w-40" value={on} max={localToday()} onChange={(e) => setOn(e.target.value)} />
            </div>
          </div>
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("valuables.saveFailed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || save.isPending} onClick={() => value && save.mutate({ metal, price: value, on }, { onSuccess: onClose })}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
