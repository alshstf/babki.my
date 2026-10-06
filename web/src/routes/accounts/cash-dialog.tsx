import { useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { AmountField, OperationDateField } from "@/components/form-fields";
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
  minorToInput,
  parseToMinor,
} from "@/lib/money";
import { localToday } from "@/lib/dates";
import {
  useSaveOperation,
  isConflict,
  type Operation,
  type OperationType,
} from "@/api/operations";
import type { AccountWithBalance } from "@/api/accounts";
import { MAX_NOTE } from "@/lib/text-limits";
import { submitOnEnter } from "@/lib/submit-on-enter";
import { useOnOpen } from "@/lib/use-on-open";

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
  editing,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
  // The recorded operation this dialog was opened on, to be corrected in place.
  editing?: Operation;
}) {
  const { t } = useTranslation();
  const createOperation = useSaveOperation(editing?.id);

  const [type, setType] = useState<OperationType>("deposit");
  const [amount, setAmount] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");

  useOnOpen(open, () => {
    setType(editing?.type ?? "deposit");
    setAmount(editing ? minorToInput(editing.amount_minor) : "");
    setOccurredOn(editing?.occurred_on ?? localToday());
    setNote(editing?.note ?? "");
    createOperation.reset();
  });

  const isCredit = CREDIT_TYPES.has(type);
  const parsed = parseToMinor(amount);
  const amountValid = parsed !== null && parsed > 0;
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
      <DialogContent className="sm:max-w-sm" onKeyDown={submitOnEnter(submit, valid && !createOperation.isPending)}>
        <DialogHeader>
          <DialogTitle>{editing ? t("operations.editTitle") : t("cash.title")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="cash-type">{t("cash.type")}</Label>
            <Select
              value={type}
              onValueChange={(v) => setType(v as OperationType)}
              disabled={editing !== undefined}
            >
              <SelectTrigger id="cash-type"><SelectValue /></SelectTrigger>
              <SelectContent>
                {CASH_TYPES.map((cashType) => (
                  <SelectItem key={cashType} value={cashType}>
                    {t(`operationTypes.${cashType}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <AmountField
            id="cash-amount"
            label={t("cash.amount", { currency: account.currency })}
            value={amount}
            onChange={setAmount}
            currency={account.currency}
            accepted={amountValid}
            badNumber={t("cash.badNumber")}
            hint={
              <p className="text-xs text-muted-foreground">
                {isCredit ? t("cash.creditHint") : t("cash.debitHint")}
              </p>
            }
          />
          <OperationDateField id="cash-date" label={t("cash.date")} value={occurredOn} onChange={setOccurredOn} />
          <div className="grid gap-2">
            <Label htmlFor="cash-note">{t("cash.note")}</Label>
            <Input id="cash-note" maxLength={MAX_NOTE} value={note} onChange={(e) => setNote(e.target.value)} />
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
