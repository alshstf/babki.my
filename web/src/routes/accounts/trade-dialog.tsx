import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { formatMinor, multiplyToMinor, parseToMinor, isPositiveDecimal } from "@/lib/money";
import { localToday } from "@/lib/dates";
import { useCreateOperation, isConflict } from "@/api/operations";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";
import { InstrumentPicker } from "./instrument-picker";

export function TradeDialog({
  open,
  onOpenChange,
  account,
  side,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
  side: "buy" | "sell";
}) {
  const { t } = useTranslation();
  const createOperation = useCreateOperation();

  const [instrument, setInstrument] = useState<Instrument | null>(null);
  const [quantity, setQuantity] = useState("");
  const [price, setPrice] = useState("");
  const [fee, setFee] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");

  useEffect(() => {
    if (open) {
      setInstrument(null);
      setQuantity("");
      setPrice("");
      setFee("");
      setOccurredOn(localToday());
      setNote("");
      createOperation.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, side]);

  const qtyValid = isPositiveDecimal(quantity);
  const priceValid = isPositiveDecimal(price);
  const feeParsed = fee.trim() === "" ? 0 : parseToMinor(fee);
  const feeValid = feeParsed !== null && feeParsed >= 0;

  // Total is computed with exact BigInt arithmetic (see multiplyToMinor) —
  // never as a float — so what's previewed here is exactly what gets sent.
  const totalMinor = qtyValid && priceValid ? multiplyToMinor(quantity, price) : null;
  const overflow = qtyValid && priceValid && totalMinor === null;

  const valid =
    instrument !== null &&
    qtyValid &&
    priceValid &&
    totalMinor !== null &&
    feeValid &&
    occurredOn !== "";

  const submit = () => {
    if (!instrument || totalMinor === null || feeParsed === null) return;
    const signedAmount = side === "buy" ? -totalMinor : totalMinor;
    createOperation.mutate(
      {
        account_id: account.id,
        instrument_id: instrument.id,
        type: side,
        occurred_on: occurredOn,
        quantity,
        price,
        amount_minor: signedAmount,
        // The operation's currency follows the instrument being traded (a
        // position's currency is fixed by its first operation and must stay
        // consistent thereafter — see internal/portfolio/engine.go), not the
        // account's own currency, which can differ (e.g. a USD-denominated
        // fund held inside a RUB brokerage account).
        currency: instrument.currency,
        fee_minor: feeParsed,
        note,
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  const errorMessage = createOperation.isError
    ? isConflict(createOperation.error)
      ? t("trade.oversell")
      : t("app.error")
    : null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            {side === "buy" ? t("trade.buyTitle") : t("trade.sellTitle")}
          </DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>{t("instrumentPicker.search")}</Label>
            <InstrumentPicker value={instrument} onChange={setInstrument} />
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="trade-qty">{t("trade.quantity")}</Label>
              <Input
                id="trade-qty"
                inputMode="decimal"
                value={quantity}
                onChange={(e) => setQuantity(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="trade-price">
                {t("trade.price")}
                {instrument && ` (${instrument.currency})`}
              </Label>
              <Input
                id="trade-price"
                inputMode="decimal"
                value={price}
                onChange={(e) => setPrice(e.target.value)}
              />
            </div>
          </div>
          {(quantity !== "" || price !== "") && !overflow && (!qtyValid || !priceValid) && (
            <p className="text-xs text-red-500">{t("trade.badNumber")}</p>
          )}
          {overflow && <p className="text-xs text-red-500">{t("trade.badNumber")}</p>}
          <div className="grid gap-2">
            <Label htmlFor="trade-fee">{t("trade.fee")}</Label>
            <Input
              id="trade-fee"
              inputMode="decimal"
              placeholder="0"
              value={fee}
              onChange={(e) => setFee(e.target.value)}
            />
            {fee !== "" && !feeValid && (
              <p className="text-xs text-red-500">{t("trade.badFee")}</p>
            )}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="trade-date">{t("trade.date")}</Label>
            <Input
              id="trade-date"
              type="date"
              value={occurredOn}
              max={localToday()}
              onChange={(e) => setOccurredOn(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="trade-note">{t("trade.note")}</Label>
            <Input id="trade-note" value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
          {totalMinor !== null && instrument && (
            <div className="rounded-lg border bg-muted/50 px-2.5 py-2 text-sm">
              <span className="text-muted-foreground">{t("trade.total")}: </span>
              <span className="font-medium tabular-nums">
                {formatMinor(totalMinor, instrument.currency)}
              </span>
            </div>
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
          <Button disabled={!valid || createOperation.isPending} onClick={submit}>
            {side === "buy" ? t("trade.buyTitle") : t("trade.sellTitle")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
