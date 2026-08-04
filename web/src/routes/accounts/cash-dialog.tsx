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
import { localToday } from "@/lib/dates";
import { useCreateOperation, isConflict, type OperationType } from "@/api/operations";
import type { AccountWithBalance } from "@/api/accounts";

// Cash-level journal entries: no instrument attribution, only a signed cash
// effect on the account. The backend enforces the sign strictly per type
// (see internal/operation/service.go validate()) — CREDIT_TYPES must be
// positive, everything else here must be negative — so the user only ever
// types a positive number and this dialog applies the correct sign.
const CASH_TYPES: OperationType[] = ["deposit", "withdrawal", "fee", "tax", "interest"];
const CREDIT_TYPES = new Set<OperationType>(["deposit", "interest"]);

export function CashDialog({
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

  const [type, setType] = useState<OperationType>("deposit");
  const [amount, setAmount] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");

  useEffect(() => {
    if (open) {
      setType("deposit");
      setAmount("");
      setOccurredOn(localToday());
      setNote("");
      createOperation.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const isCredit = CREDIT_TYPES.has(type);
  const parsed = parseToMinor(amount);
  const amountValid = parsed !== null && parsed > 0;
  // A sum past the bound is positive and parses perfectly well, so «введите
  // положительную сумму» would name a cause that is not the cause (see
  // AmountRefusal).
  const refusal = amountRefusal(amount);
  const valid = amountValid && occurredOn !== "";

  const submit = () => {
    if (!amountValid || parsed === null) return;
    createOperation.mutate(
      {
        account_id: account.id,
        type,
        occurred_on: occurredOn,
        amount_minor: isCredit ? parsed : -parsed,
        currency: account.currency,
        note,
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>{t("cash.title")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>{t("cash.type")}</Label>
            <Select value={type} onValueChange={(v) => setType(v as OperationType)}>
              <SelectTrigger><SelectValue /></SelectTrigger>
              <SelectContent>
                {CASH_TYPES.map((cashType) => (
                  <SelectItem key={cashType} value={cashType}>
                    {t(`operationTypes.${cashType}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cash-amount">
              {t("cash.amount", { currency: account.currency })}
            </Label>
            <Input
              id="cash-amount"
              inputMode="decimal"
              placeholder="0"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">
              {isCredit ? t("cash.creditHint") : t("cash.debitHint")}
            </p>
            {amount !== "" && !amountValid && (
              <p className="text-xs text-red-500">
                {refusal === "tooLarge"
                  ? t("common.amountTooLarge", {
                      max: formatMinorCompact(MAX_AMOUNT_MINOR, account.currency),
                    })
                  : t("cash.badNumber")}
              </p>
            )}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cash-date">{t("cash.date")}</Label>
            <Input
              id="cash-date"
              type="date"
              value={occurredOn}
              max={localToday()}
              onChange={(e) => setOccurredOn(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cash-note">{t("cash.note")}</Label>
            <Input id="cash-note" value={note} onChange={(e) => setNote(e.target.value)} />
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
