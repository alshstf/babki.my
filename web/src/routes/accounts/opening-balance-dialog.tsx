import { useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { AmountField } from "@/components/form-fields";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { formatMinor, minorToInput, parseToMinor, MAX_AMOUNT_MINOR } from "@/lib/money";
import { formatDate, formatDateTime, localToday } from "@/lib/dates";
import { useSaveOperation, isConflict } from "@/api/operations";
import { useConnections, type TinvestAccountReconcile } from "@/api/connections";
import type { AccountWithBalance } from "@/api/accounts";
import type { CashPosition } from "@/api/positions";
import { submitOnEnter } from "@/lib/submit-on-enter";
import { useOnOpen } from "@/lib/use-on-open";

// What the broker's last check said this account holds in currency, in minor
// units, or null when the account is not linked, was never checked, or the
// check found the money matching (then the journal is not short of it).
function brokerCashMinor(
  reconcile: TinvestAccountReconcile | undefined,
  currency: string,
): number | null {
  const row = reconcile?.mismatches.find(
    (m) => m.kind === "currency" && m.label.toUpperCase() === currency,
  );
  // The broker's figure is a decimal of any length; the journal keeps
  // hundredths, so the rest is cut, never rounded up into money not there.
  const match = row?.broker.match(/^(\d+)(?:\.(\d+))?$/);
  if (!match) return null;
  const [, whole, fraction = ""] = match;
  return parseToMinor(fraction ? `${whole}.${fraction.slice(0, 2)}` : whole);
}

// The opening balance (decision Р-2, variant Б): a journal begun after the
// account already held money goes below zero. One deposit closes it — what the
// account holds now less what the journal says — dated the day the journal
// first went short. It is an ordinary row, corrected or deleted like any other.
export function OpeningBalanceDialog({
  open,
  onOpenChange,
  account,
  money,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
  // A cash row with a negative balance and the day it first went short.
  money: CashPosition & { overdrawn_since: string };
}) {
  const { t } = useTranslation();
  const save = useSaveOperation();
  const connections = useConnections();
  const reconcile = connections.data
    ?.flatMap((c) => c.reconciles)
    .find((r) => r.account_id === account.id);
  const fromBroker = brokerCashMinor(reconcile, money.currency);

  // Null until the person types: the broker's figure, which may arrive after
  // the dialog opens, stands in for it.
  const [typed, setTyped] = useState<string | null>(null);
  useOnOpen(open, () => {
    setTyped(null);
    save.reset();
  });
  const held = typed ?? (fromBroker === null ? "" : minorToInput(fromBroker));

  const parsed = parseToMinor(held);
  const deposit = parsed === null ? null : parsed - money.amount_minor;
  const valid =
    parsed !== null && parsed >= 0 && deposit !== null && deposit > 0 && deposit <= MAX_AMOUNT_MINOR;

  const submit = () => {
    if (!valid || parsed === null || deposit === null) return;
    save.mutate(
      {
        account_id: account.id,
        type: "deposit",
        occurred_on: money.overdrawn_since,
        amount_minor: deposit,
        currency: money.currency,
        note: t("openingBalance.note", {
          held: formatMinor(parsed, money.currency),
          on: formatDate(localToday()),
        }),
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md" onKeyDown={submitOnEnter(submit, valid && !save.isPending)}>
        <DialogHeader>
          <DialogTitle>{t("openingBalance.title", { currency: money.currency })}</DialogTitle>
          <DialogDescription>
            {t("openingBalance.why", {
              balance: formatMinor(money.amount_minor, money.currency),
              since: formatDate(money.overdrawn_since),
            })}
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4">
          <AmountField
            id="opening-held"
            label={t("openingBalance.held", { currency: money.currency })}
            value={held}
            onChange={setTyped}
            currency={money.currency}
            accepted={parsed !== null && parsed >= 0}
            badNumber={t("openingBalance.badNumber")}
            hint={
              typed === null && fromBroker !== null && reconcile?.at ? (
                <p className="text-xs text-muted-foreground" data-testid="opening-from-broker">
                  {t("openingBalance.fromBroker", { at: formatDateTime(reconcile.at) })}
                </p>
              ) : undefined
            }
          />
          {reconcile && reconcile.currency_trades_unparsed > 0 && (
            <Alert data-testid="opening-unparsed">
              <AlertDescription>
                {t("openingBalance.unparsedCurrencyTrades", { count: reconcile.currency_trades_unparsed })}
              </AlertDescription>
            </Alert>
          )}
          {valid && deposit !== null && (
            <p className="text-sm" data-testid="opening-preview">
              {t("openingBalance.preview", {
                amount: formatMinor(deposit, money.currency),
                on: formatDate(money.overdrawn_since),
              })}
            </p>
          )}
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(save.error) ? t("operations.conflict") : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || save.isPending} onClick={submit}>
            {t("openingBalance.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
