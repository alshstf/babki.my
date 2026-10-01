import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { QueryGate } from "@/components/query-notice";
import { queryState } from "@/lib/query-state";
import { formatDate } from "@/lib/dates";
import { formatMinor } from "@/lib/money";
import { ApiError, isConflict } from "@/api/operations";
import { useAccounts } from "@/api/accounts";
import { useArrivals, useStatePurchases, type Arrival } from "@/api/arrivals";
import {
  PurchasesEditor,
  newPurchaseRow,
  purchasesReady,
  toStatedPurchase,
  type PurchaseRow,
} from "./purchases-editor";

export type PricedPaper = { id: string; name: string; ticker: string };

// PurchasePriceDialog is where the owner gives shares that arrived without a
// purchase price their price — the action behind the paper's «цена покупки
// неизвестна» note. It lists every transfer of the paper into the account:
// one from another broker can be given its purchases here; one moved from
// another of the owner's accounts carries that account's purchases, so it
// points there.
export function PurchasePriceDialog({
  open,
  onOpenChange,
  accountId,
  paper,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  accountId: string;
  paper: PricedPaper;
}) {
  const { t } = useTranslation();
  const arrivals = useArrivals(accountId, paper.id, open);
  const accounts = useAccounts();
  const state = queryState(arrivals);
  const accountName = (id: string | null | undefined) =>
    accounts.data?.find((a) => a.id === id)?.name ?? t("purchases.unknownAccount");

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>{t("purchases.title", { name: paper.name })}</DialogTitle>
          <DialogDescription>{t("purchases.intro")}</DialogDescription>
        </DialogHeader>
        {state !== "ready" ? (
          <QueryGate state={state} />
        ) : arrivals.data && arrivals.data.length > 0 ? (
          <div className="grid gap-4">
            {arrivals.data.map((arrival) =>
              arrival.from_another_broker ? (
                <ArrivalPurchases key={arrival.operation_id} accountId={accountId} arrival={arrival} />
              ) : (
                <div
                  key={arrival.operation_id}
                  className="rounded-lg border p-3 text-sm"
                  data-testid="arrival-from-account"
                >
                  <div className="font-medium">
                    {t("purchases.arrived", { date: formatDate(arrival.occurred_on), quantity: arrival.quantity })}
                  </div>
                  <p className="text-muted-foreground">
                    {t("purchases.fromAccount", { name: accountName(arrival.from_account_id) })}{" "}
                    {arrival.from_account_id && (
                      <Link
                        to="/accounts/$accountId"
                        params={{ accountId: arrival.from_account_id }}
                        className="underline"
                        onClick={() => onOpenChange(false)}
                      >
                        {t("purchases.goToAccount")}
                      </Link>
                    )}
                  </p>
                </div>
              ),
            )}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground" data-testid="no-arrivals">
            {t("purchases.noArrivals")}
          </p>
        )}
      </DialogContent>
    </Dialog>
  );
}

// ArrivalPurchases is one arrival from another broker: what is recorded behind
// it, and the form that replaces that.
function ArrivalPurchases({ accountId, arrival }: { accountId: string; arrival: Arrival }) {
  const { t } = useTranslation();
  const statePurchases = useStatePurchases(accountId);
  const [rows, setRows] = useState<PurchaseRow[]>(() => [newPurchaseRow(arrival.quantity)]);
  const ready = purchasesReady(rows, arrival.occurred_on, arrival.quantity);
  const idPrefix = `arrival-${arrival.operation_id}`;

  const save = () => {
    if (!ready) return;
    statePurchases.mutate({ operationId: arrival.operation_id, purchases: rows.map(toStatedPurchase) });
  };

  // The status is all this reads (see isConflict): 409 is the journal refusing
  // the purchases, 400 the request — both the owner's to correct — and
  // anything else a failure whose cause the screen was not told.
  const errorMessage = statePurchases.isError
    ? isConflict(statePurchases.error)
      ? t("purchases.conflict")
      : statePurchases.error instanceof ApiError && statePurchases.error.status === 400
        ? t("purchases.refused")
        : t("app.error")
    : null;

  return (
    <div className="grid gap-3 rounded-lg border p-3" data-testid="arrival-from-another-broker">
      <div>
        <div className="font-medium">
          {t("purchases.arrived", { date: formatDate(arrival.occurred_on), quantity: arrival.quantity })}
        </div>
        {arrival.purchases.length > 0 ? (
          <ul className="text-sm text-muted-foreground" data-testid={`${idPrefix}-recorded`}>
            {arrival.purchases.map((purchase, index) => (
              <li key={index}>
                {purchase.acquired_on
                  ? t("purchases.recordedOn", {
                      quantity: purchase.quantity,
                      cost: formatMinor(purchase.cost_minor, arrival.currency),
                      date: formatDate(purchase.acquired_on),
                    })
                  : t("purchases.recordedUndated", {
                      quantity: purchase.quantity,
                      cost: formatMinor(purchase.cost_minor, arrival.currency),
                    })}
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-sm text-amber-600">{t("purchases.notRecorded")}</p>
        )}
      </div>
      <PurchasesEditor
        rows={rows}
        onChange={setRows}
        currency={arrival.currency}
        arrivedOn={arrival.occurred_on}
        expected={arrival.quantity}
        idPrefix={idPrefix}
      />
      {errorMessage && (
        <Alert variant="destructive">
          <AlertDescription>{errorMessage}</AlertDescription>
        </Alert>
      )}
      {statePurchases.isSuccess && !statePurchases.isPending && (
        <p className="text-sm text-emerald-600" data-testid={`${idPrefix}-saved`}>
          {t("purchases.saved")}
        </p>
      )}
      <div className="flex justify-end">
        <Button disabled={!ready || statePurchases.isPending} onClick={save}>
          {arrival.purchases.length > 0 ? t("purchases.replace") : t("common.save")}
        </Button>
      </div>
    </div>
  );
}
