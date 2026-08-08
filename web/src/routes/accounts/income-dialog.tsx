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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  MAX_AMOUNT_MINOR,
  amountRefusal,
  formatMinorCompact,
  parseToMinor,
} from "@/lib/money";
import { EARLIEST_OPERATION_DATE, localToday } from "@/lib/dates";
import { useCreateOperation, isConflict, type OperationType } from "@/api/operations";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";
import { InstrumentPicker } from "./instrument-picker";

// Dividend and coupon may be recorded at the cash level (no instrument) per
// the backend's validation contract (Type.RequiresInstrument in
// internal/portfolio/operation.go); amortization always needs one.
const INCOME_TYPES: OperationType[] = ["dividend", "coupon", "amortization"];
const REQUIRES_INSTRUMENT = new Set<OperationType>(["amortization"]);

// The one type this form records that the ledger does not treat as income
// (#109). A dividend and a coupon add to Position.IncomeByCurrency; an
// amortization is written as a DISPOSAL instead — the engine calls
// p.realize() with the returned principal as proceeds and the basis it
// retires as the released pieces, and never touches income (the
// TypeAmortization branch in internal/portfolio/engine.go). So it never
// reaches the «Доход» column of the positions table, at any size, and this
// dialog used to be titled «Доход по инструменту» over a picker offering it.
//
// The remedy is a narrowing plus a sentence, not a second dialog. The title
// now says «Выплата», which is true of all three — a dividend, a coupon and a
// return of principal are all money the issuer pays out — and the note below
// appears for the one type whose destination would otherwise be guessed
// wrong. It says where the entry GOES rather than what it will be worth: an
// amortization's result in the position's own currency is the excess over
// whatever basis is left, which is usually zero, so promising a figure would
// be its own false caption.
const NOT_INCOME = new Set<OperationType>(["amortization"]);

export function IncomeDialog({
  open,
  onOpenChange,
  account,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
}) {
  const { t } = useTranslation();
  const createOperation = useCreateOperation();

  const [type, setType] = useState<OperationType>("dividend");
  const [instrument, setInstrument] = useState<Instrument | null>(null);
  const [amount, setAmount] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");

  useEffect(() => {
    if (open) {
      setType("dividend");
      setInstrument(null);
      setAmount("");
      setOccurredOn(localToday());
      setNote("");
      createOperation.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const parsed = parseToMinor(amount);
  const amountValid = parsed !== null && parsed > 0;
  // A sum past the bound is positive and parses perfectly well, so neither of
  // this field's two existing complaints would be true of it (see
  // AmountRefusal).
  const refusal = amountRefusal(amount);
  const instrumentOk = instrument !== null || !REQUIRES_INSTRUMENT.has(type);
  const valid = amountValid && instrumentOk && occurredOn !== "";

  const submit = () => {
    if (!amountValid || parsed === null || !instrumentOk) return;
    createOperation.mutate(
      {
        account_id: account.id,
        instrument_id: instrument?.id,
        type,
        occurred_on: occurredOn,
        amount_minor: parsed,
        // Follows the instrument's currency when one is attributed; falls
        // back to the account's own currency for a cash-level entry.
        currency: instrument ? instrument.currency : account.currency,
        note,
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("income.title")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>{t("income.type")}</Label>
            <Select value={type} onValueChange={(v) => setType(v as OperationType)}>
              <SelectTrigger><SelectValue /></SelectTrigger>
              <SelectContent>
                {INCOME_TYPES.map((incomeType) => (
                  <SelectItem key={incomeType} value={incomeType}>
                    {t(`operationTypes.${incomeType}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {NOT_INCOME.has(type) && (
              <p
                data-testid="income-amortization-note"
                className="text-xs text-muted-foreground"
              >
                {t("income.amortizationNote")}
              </p>
            )}
          </div>
          <div className="grid gap-2">
            <div className="flex items-center justify-between">
              <Label>
                {REQUIRES_INSTRUMENT.has(type)
                  ? t("instrumentPicker.search")
                  : t("income.instrumentOptional")}
              </Label>
              {instrument && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => setInstrument(null)}
                >
                  {t("income.clearInstrument")}
                </Button>
              )}
            </div>
            <InstrumentPicker value={instrument} onChange={setInstrument} />
            {!instrumentOk && (
              <p className="text-xs text-red-500">{t("income.instrumentRequired")}</p>
            )}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="income-amount">
              {t("income.amount", { currency: instrument ? instrument.currency : account.currency })}
            </Label>
            <Input
              id="income-amount"
              inputMode="decimal"
              placeholder="0"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
            />
            {amount !== "" && !amountValid && (
              <p className="text-xs text-red-500">
                {refusal === "tooLarge"
                  ? t("common.amountTooLarge", {
                      max: formatMinorCompact(
                        MAX_AMOUNT_MINOR,
                        instrument ? instrument.currency : account.currency,
                      ),
                    })
                  : t("income.badNumber")}
              </p>
            )}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="income-date">{t("income.date")}</Label>
            <Input
              id="income-date"
              type="date"
              value={occurredOn}
              min={EARLIEST_OPERATION_DATE}
              max={localToday()}
              onChange={(e) => setOccurredOn(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="income-note">{t("income.note")}</Label>
            <Input id="income-note" value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
          {createOperation.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(createOperation.error) ? t("operations.conflict") : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || createOperation.isPending} onClick={submit}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
