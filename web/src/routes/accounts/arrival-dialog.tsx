import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { EARLIEST_OPERATION_DATE, localToday } from "@/lib/dates";
import { isPositiveDecimal } from "@/lib/money";
import { normalizeQuantity } from "@/lib/quantity";
import { ApiError, isConflict } from "@/api/operations";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";
import { useCreateArrival } from "@/api/arrivals";
import { InstrumentPicker } from "./instrument-picker";
import {
  PurchasesEditor,
  newPurchaseRow,
  purchasesReady,
  toStatedPurchase,
  type PurchaseRow,
} from "./purchases-editor";

// ArrivalDialog records shares that came from another broker — one this
// program does not hold — on an account no importer feeds. With the purchases
// behind them when the owner has them; without, they arrive bought for nothing
// and the paper says so until they are given.
export function ArrivalDialog({
  open,
  onOpenChange,
  account,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
}) {
  const { t } = useTranslation();
  const createArrival = useCreateArrival();
  const [instrument, setInstrument] = useState<Instrument | null>(null);
  const [quantity, setQuantity] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");
  const [priced, setPriced] = useState(false);
  const [rows, setRows] = useState<PurchaseRow[]>(() => [newPurchaseRow()]);

  useEffect(() => {
    if (open) {
      setInstrument(null);
      setQuantity("");
      setOccurredOn(localToday());
      setNote("");
      setPriced(false);
      setRows([newPurchaseRow()]);
      createArrival.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const qty = normalizeQuantity(quantity);
  const qtyValid = isPositiveDecimal(qty);
  const valid =
    instrument !== null &&
    qtyValid &&
    occurredOn !== "" &&
    (!priced || purchasesReady(rows, occurredOn, qty));

  const submit = () => {
    if (!instrument || !valid) return;
    createArrival.mutate(
      {
        account_id: account.id,
        instrument_id: instrument.id,
        occurred_on: occurredOn,
        quantity: qty,
        currency: instrument.currency,
        note,
        purchases: priced ? rows.map(toStatedPurchase) : [],
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  const errorMessage = createArrival.isError
    ? isConflict(createArrival.error)
      ? t("arrival.conflict")
      : createArrival.error instanceof ApiError && createArrival.error.status === 400
        ? t("arrival.refused")
        : t("app.error")
    : null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>{t("arrival.title")}</DialogTitle>
          <DialogDescription>{t("arrival.intro")}</DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>{t("instrumentPicker.search")}</Label>
            <InstrumentPicker value={instrument} onChange={setInstrument} />
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="arrival-qty">{t("arrival.quantity")}</Label>
              <Input
                id="arrival-qty"
                inputMode="decimal"
                value={quantity}
                onChange={(e) => setQuantity(e.target.value)}
              />
              {quantity !== "" && !qtyValid && (
                <p className="text-xs text-red-500">{t("arrival.badQuantity")}</p>
              )}
            </div>
            <div className="grid gap-2">
              <Label htmlFor="arrival-date">{t("arrival.date")}</Label>
              <Input
                id="arrival-date"
                type="date"
                value={occurredOn}
                min={EARLIEST_OPERATION_DATE}
                max={localToday()}
                onChange={(e) => setOccurredOn(e.target.value)}
              />
            </div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="arrival-note">{t("arrival.note")}</Label>
            <Input id="arrival-note" value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
          <label className="flex items-center gap-2 text-sm" htmlFor="arrival-priced">
            <Checkbox id="arrival-priced" checked={priced} onCheckedChange={(v) => setPriced(v === true)} />
            {t("arrival.knowPrice")}
          </label>
          {priced ? (
            instrument && qtyValid ? (
              <PurchasesEditor
                rows={rows}
                onChange={setRows}
                currency={instrument.currency}
                arrivedOn={occurredOn}
                expected={qty}
                idPrefix="new-arrival"
              />
            ) : (
              <p className="text-xs text-muted-foreground">{t("arrival.pickFirst")}</p>
            )
          ) : (
            <p className="text-xs text-amber-600">{t("arrival.unpriced")}</p>
          )}
          {errorMessage && (
            <Alert variant="destructive">
              <AlertDescription>{errorMessage}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || createArrival.isPending} onClick={submit}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
