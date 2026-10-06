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
import { AmountField } from "@/components/form-fields";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  amountRefusal,
  formatMinor,
  parseToMinor,
} from "@/lib/money";
import { formatDate, localToday } from "@/lib/dates";
import { useSetBalance, type AccountWithBalance } from "@/api/accounts";
import { submitOnEnter } from "@/lib/submit-on-enter";
import { useOnOpen } from "@/lib/use-on-open";

export function BalanceDialog({
  open,
  onOpenChange,
  account,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance | undefined;
}) {
  const { t } = useTranslation();
  const setBalance = useSetBalance();
  const [amount, setAmount] = useState("");
  const [asOf, setAsOf] = useState(localToday());

  useOnOpen(open, () => {
    setAmount("");
    setAsOf(localToday());
    setBalance.reset();
  });

  if (!account) return null;
  const parsed = parseToMinor(amount);
  // Why the field cannot send what is in it, when it cannot. A sum past the
  // bound parses perfectly well, so answering it with the parse error would name
  // a cause that is not the cause — see AmountRefusal.
  const refusal = amountRefusal(amount);
  const isLiability = account.type === "credit_card" || account.type === "loan";
  // A balance is about a day already over; the server refuses later dates
  // (parseAsOf), and the input's `max` bounds only its arrows (#95). Compared as
  // strings (YYYY-MM-DD). Against the local today; the server allows UTC-today + 1,
  // so nothing accepted here is refused there.
  const today = localToday();
  const futureDate = asOf !== "" && asOf > today;

  const canSave = parsed !== null && !!asOf && !futureDate && !setBalance.isPending;
  const save = () => {
    if (parsed === null) return;
    setBalance.mutate({ id: account.id, asOf, amountMinor: parsed }, { onSuccess: () => onOpenChange(false) });
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm" onKeyDown={submitOnEnter(save, canSave)}>
        <DialogHeader>
          <DialogTitle>
            {t("accounts.balanceDialog.title", { name: account.name })}
          </DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          {account.balance && (
            <p className="text-sm text-muted-foreground">
              {t("accounts.balanceDialog.current")}:{" "}
              {formatMinor(account.balance.amount_minor, account.currency)} (
              {formatDate(account.balance.as_of)})
            </p>
          )}
          <AmountField
            id="bal-amount"
            label={t("accounts.balanceDialog.amount", { currency: account.currency })}
            value={amount}
            onChange={setAmount}
            currency={account.currency}
            accepted={refusal === null}
            badNumber={t("accounts.balanceDialog.parseError")}
            placeholder={isLiability ? "-45 000" : "150 000,50"}
            hint={
              isLiability && (
                <p className="text-xs text-muted-foreground">
                  {t("accounts.balanceDialog.liabilityHint")}
                </p>
              )
            }
          />
          <div className="grid gap-2">
            <Label htmlFor="bal-date">{t("accounts.balanceDialog.date")}</Label>
            <Input
              id="bal-date"
              type="date"
              value={asOf}
              max={today}
              onChange={(e) => setAsOf(e.target.value)}
            />
            {futureDate && (
              <p className="text-xs text-red-500">
                {t("accounts.balanceDialog.futureDate")}
              </p>
            )}
          </div>
          {/* The client knows only that the save did not happen; a future date,
             the one cause a reader can act on, is refused at the field. */}
          {setBalance.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("accounts.balanceDialog.saveError")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!canSave} onClick={save}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
