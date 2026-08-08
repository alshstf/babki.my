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
import { EARLIEST_OPERATION_DATE, localToday } from "@/lib/dates";
import { isPositiveDecimal } from "@/lib/money";
import { useAccounts, type AccountWithBalance } from "@/api/accounts";
import { useCreateTransfer, isConflict } from "@/api/operations";
import type { Instrument } from "@/api/instruments";
import { InstrumentPicker } from "./instrument-picker";

export function TransferDialog({
  open,
  onOpenChange,
  account,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
}) {
  const { t } = useTranslation();
  const accounts = useAccounts();
  const createTransfer = useCreateTransfer();

  const [instrument, setInstrument] = useState<Instrument | null>(null);
  const [toAccountId, setToAccountId] = useState("");
  const [quantity, setQuantity] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");

  useEffect(() => {
    if (open) {
      setInstrument(null);
      setToAccountId("");
      setQuantity("");
      setOccurredOn(localToday());
      setNote("");
      createTransfer.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // A transfer only makes sense between two active brokerage accounts
  // (positions live there); the current account is excluded so the picker
  // can't offer an obviously-invalid same-account target.
  const targetAccounts = (accounts.data ?? []).filter(
    (a) => a.id !== account.id && a.type === "brokerage" && a.status === "active",
  );

  const qtyValid = isPositiveDecimal(quantity);
  const sameAccount = toAccountId !== "" && toAccountId === account.id;
  const valid =
    instrument !== null &&
    toAccountId !== "" &&
    !sameAccount &&
    qtyValid &&
    occurredOn !== "";

  const submit = () => {
    if (!instrument || !valid) return;
    createTransfer.mutate(
      {
        from_account_id: account.id,
        to_account_id: toAccountId,
        instrument_id: instrument.id,
        quantity,
        occurred_on: occurredOn,
        note,
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  // The same defect #23 found in the trade dialog, on the endpoint next door,
  // and fixed here because the contract now states the shared rule out loud
  // (see /api/v1/operations/transfer in api/openapi.yaml): 409 says a replay
  // refused and nothing finer. «Недостаточно бумаг для переноса» named one cause
  // of several — a currency that does not match the position's, a stored FIFO
  // breakdown that no longer matches the history it is replayed against, a
  // journal that already did not compute — and it named the wrong ACCOUNT half
  // the time as well: both journals are replayed, so the row refused can be one
  // on the receiving side, which has nothing to do with what the source holds.
  //
  // A sentence of its own rather than operations.conflict's, because the fact IS
  // different here: two accounts, and no telling which of them the refusal came
  // from.
  const errorMessage = createTransfer.isError
    ? isConflict(createTransfer.error)
      ? t("transfer.conflict")
      : t("app.error")
    : null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("transfer.title")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>{t("instrumentPicker.search")}</Label>
            <InstrumentPicker value={instrument} onChange={setInstrument} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="transfer-qty">{t("transfer.quantity")}</Label>
            <Input
              id="transfer-qty"
              inputMode="decimal"
              value={quantity}
              onChange={(e) => setQuantity(e.target.value)}
            />
            {quantity !== "" && !qtyValid && (
              <p className="text-xs text-red-500">{t("transfer.badNumber")}</p>
            )}
          </div>
          <div className="grid gap-2">
            <Label>{t("transfer.toAccount")}</Label>
            <Select value={toAccountId} onValueChange={setToAccountId}>
              <SelectTrigger className="w-full">
                <SelectValue placeholder={t("transfer.toAccount")} />
              </SelectTrigger>
              <SelectContent>
                {targetAccounts.map((target) => (
                  <SelectItem key={target.id} value={target.id}>
                    {target.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {targetAccounts.length === 0 && (
              <p className="text-xs text-muted-foreground">{t("transfer.noTargets")}</p>
            )}
            {sameAccount && <p className="text-xs text-red-500">{t("transfer.sameAccount")}</p>}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="transfer-date">{t("transfer.date")}</Label>
            <Input
              id="transfer-date"
              type="date"
              value={occurredOn}
              min={EARLIEST_OPERATION_DATE}
              max={localToday()}
              onChange={(e) => setOccurredOn(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="transfer-note">{t("transfer.note")}</Label>
            <Input id="transfer-note" value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
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
          <Button disabled={!valid || createTransfer.isPending} onClick={submit}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
