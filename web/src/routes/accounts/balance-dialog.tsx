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
import {
  MAX_AMOUNT_MINOR,
  amountRefusal,
  formatMinor,
  formatMinorCompact,
  parseToMinor,
} from "@/lib/money";
import { formatDate, localToday } from "@/lib/dates";
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
  // Why the field cannot send what is in it, when it cannot. A sum past the
  // bound parses perfectly well, so answering it with the parse error would name
  // a cause that is not the cause — see AmountRefusal.
  const refusal = amountRefusal(amount);
  const isLiability = account.type === "credit_card" || account.type === "loan";
  // A balance is a statement about a day that has already happened, and the
  // server refuses one dated later (parseAsOf in internal/account/http.go). The
  // field's own `max` below only bounds the picker's arrows — a date typed into
  // it goes through untouched — so before #95 the refusal came back from the
  // server and was printed in its own words, in English. Compared as strings
  // because both sides are YYYY-MM-DD, where lexical order IS calendar order.
  //
  // Against the LOCAL today, which is what `max` already promises the reader.
  // The server's own bound is a day looser (it allows UTC-today + 1, so that
  // every timezone can record its own today), so nothing this field accepts can
  // be refused there for being in the future.
  const today = localToday();
  const futureDate = asOf !== "" && asOf > today;

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
              {formatDate(account.balance.as_of)})
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
            {amount !== "" && refusal !== null && (
              <p className="text-xs text-red-500">
                {refusal === "tooLarge"
                  ? t("common.amountTooLarge", {
                      max: formatMinorCompact(MAX_AMOUNT_MINOR, account.currency),
                    })
                  : t("accounts.balanceDialog.parseError")}
              </p>
            )}
          </div>
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
          {/* What the client knows about a refusal is that this save did not
              happen. WHY is the server's own business: its message is English
              prose written for a log, it is not part of the contract (only the
              status is, see api/openapi.yaml), and nothing here can translate
              a sentence it has never seen. The one cause a reader could act on
              — a date in the future — is refused above, at the field, before
              anything is sent. */}
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
          <Button
            disabled={parsed === null || !asOf || futureDate || setBalance.isPending}
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
