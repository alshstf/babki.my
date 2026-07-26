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
import { formatMinor, parseToMinor } from "@/lib/money";
import { localToday } from "@/lib/dates";
import { useSetBalance, type AccountWithBalance } from "@/api/accounts";

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

  useEffect(() => {
    if (open) {
      setAmount("");
      setAsOf(localToday());
      setBalance.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  if (!account) return null;
  const parsed = parseToMinor(amount);
  const isLiability = account.type === "credit_card" || account.type === "loan";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
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
              {account.balance.as_of})
            </p>
          )}
          <div className="grid gap-2">
            <Label htmlFor="bal-amount">
              {t("accounts.balanceDialog.amount", { currency: account.currency })}
            </Label>
            <Input
              id="bal-amount"
              inputMode="decimal"
              placeholder={isLiability ? "-45 000" : "150 000,50"}
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
            />
            {isLiability && (
              <p className="text-xs text-muted-foreground">
                {t("accounts.balanceDialog.liabilityHint")}
              </p>
            )}
            {amount !== "" && parsed === null && (
              <p className="text-xs text-red-500">
                {t("accounts.balanceDialog.parseError")}
              </p>
            )}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="bal-date">{t("accounts.balanceDialog.date")}</Label>
            <Input
              id="bal-date"
              type="date"
              value={asOf}
              max={localToday()}
              onChange={(e) => setAsOf(e.target.value)}
            />
          </div>
          {setBalance.isError && (
            <Alert variant="destructive">
              <AlertDescription>{setBalance.error.message}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button
            disabled={parsed === null || !asOf || setBalance.isPending}
            onClick={() =>
              setBalance.mutate(
                { id: account.id, asOf, amountMinor: parsed! },
                { onSuccess: () => onOpenChange(false) },
              )
            }
          >
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
