import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { OperationDateField } from "@/components/form-fields";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { MAX_AMOUNT_MINOR, amountRefusal, formatMinorCompact, parseToMinor } from "@/lib/money";
import { localToday } from "@/lib/dates";
import { isConflict, useCreateMoneyTransfer } from "@/api/operations";
import { useAccounts, type AccountWithBalance } from "@/api/accounts";
import { MAX_NOTE } from "@/lib/text-limits";
import { submitOnEnter } from "@/lib/submit-on-enter";
import { isCurrencyCode } from "@/lib/currencies";

// Money moved from this account to another of the family's: one transfer, a
// withdrawal here and a deposit there. What arrives is what left unless the
// reader says it arrived converted.
export function MoneyTransferDialog({
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
  const transfer = useCreateMoneyTransfer();
  const [toAccountId, setToAccountId] = useState("");
  const [amount, setAmount] = useState("");
  const [currency, setCurrency] = useState(account.currency);
  const [converted, setConverted] = useState(false);
  const [received, setReceived] = useState("");
  const [receivedCurrency, setReceivedCurrency] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");

  useEffect(() => {
    if (open) {
      setToAccountId("");
      setAmount("");
      setCurrency(account.currency);
      setConverted(false);
      setReceived("");
      setReceivedCurrency("");
      setOccurredOn(localToday());
      setNote("");
      transfer.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const targets = (accounts.data ?? []).filter((a) => a.id !== account.id && a.status === "active");
  const target = targets.find((a) => a.id === toAccountId);
  const parsed = parseToMinor(amount);
  const amountValid = parsed !== null && parsed > 0;
  const refusal = amountRefusal(amount);
  const currencyValid = isCurrencyCode(currency);
  const receivedParsed = parseToMinor(received);
  const receivedValid = !converted || (receivedParsed !== null && receivedParsed > 0);
  const receivedCurrencyValid = !converted || isCurrencyCode(receivedCurrency);
  const valid =
    target !== undefined && amountValid && currencyValid && receivedValid && receivedCurrencyValid && occurredOn !== "";

  const submit = () => {
    if (!valid || parsed === null) return;
    transfer.mutate(
      {
        from_account_id: account.id,
        to_account_id: toAccountId,
        occurred_on: occurredOn,
        amount_minor: parsed,
        currency,
        ...(converted && receivedParsed !== null
          ? { received_minor: receivedParsed, received_currency: receivedCurrency }
          : {}),
        note,
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm" onKeyDown={submitOnEnter(submit, valid && !transfer.isPending)}>
        <DialogHeader>
          <DialogTitle>{t("moneyTransfer.title")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>{t("moneyTransfer.toAccount")}</Label>
            {targets.length === 0 ? (
              <p className="text-sm text-muted-foreground">{t("moneyTransfer.noTargets")}</p>
            ) : (
              <Select
                value={toAccountId}
                onValueChange={(id) => {
                  setToAccountId(id);
                  const picked = targets.find((a) => a.id === id);
                  if (picked && receivedCurrency === "") setReceivedCurrency(picked.currency);
                }}
              >
                <SelectTrigger aria-label={t("moneyTransfer.toAccount")}>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {targets.map((a) => (
                    <SelectItem key={a.id} value={a.id}>
                      {a.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          </div>
          <div className="grid grid-cols-[1fr_6rem] gap-2">
            <div className="grid gap-2">
              <Label htmlFor="mt-amount">{t("moneyTransfer.amount", { currency })}</Label>
              <Input
                id="mt-amount"
                inputMode="decimal"
                placeholder="0"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="mt-currency">{t("moneyTransfer.currency")}</Label>
              <Input
                id="mt-currency"
                maxLength={3}
                value={currency}
                onChange={(e) => setCurrency(e.target.value.toUpperCase())}
              />
            </div>
          </div>
          {amount !== "" && !amountValid && (
            <p className="text-xs text-red-500">
              {refusal === "tooLarge"
                ? t("common.amountTooLarge", { max: formatMinorCompact(MAX_AMOUNT_MINOR, currency) })
                : t("cash.badNumber")}
            </p>
          )}
          {!currencyValid && <p className="text-xs text-red-500">{t("moneyTransfer.badCurrency")}</p>}
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={converted} onChange={(e) => setConverted(e.target.checked)} />
            {t("moneyTransfer.otherReceived")}
          </label>
          {converted && (
            <div className="grid grid-cols-[1fr_6rem] gap-2">
              <div className="grid gap-2">
                <Label htmlFor="mt-received">{t("moneyTransfer.received", { currency: receivedCurrency })}</Label>
                <Input
                  id="mt-received"
                  inputMode="decimal"
                  placeholder="0"
                  value={received}
                  onChange={(e) => setReceived(e.target.value)}
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="mt-received-currency">{t("moneyTransfer.receivedCurrency")}</Label>
                <Input
                  id="mt-received-currency"
                  maxLength={3}
                  value={receivedCurrency}
                  onChange={(e) => setReceivedCurrency(e.target.value.toUpperCase())}
                />
              </div>
            </div>
          )}
          <OperationDateField id="mt-date" label={t("cash.date")} value={occurredOn} onChange={setOccurredOn} />
          <div className="grid gap-2">
            <Label htmlFor="mt-note">{t("cash.note")}</Label>
            <Input id="mt-note" maxLength={MAX_NOTE} value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
          <p className="text-xs text-muted-foreground">{t("moneyTransfer.hint")}</p>
          {transfer.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(transfer.error) ? t("moneyTransfer.conflict") : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || transfer.isPending} onClick={submit}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
