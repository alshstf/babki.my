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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { amountRefusal, parseToMinor, isPositiveDecimal } from "@/lib/money";
import { EARLIEST_OPERATION_DATE, localToday } from "@/lib/dates";
import { InstrumentPicker } from "@/routes/accounts/instrument-picker";
import type { Instrument } from "@/api/instruments";
import { useExplainRows } from "@/api/explanations";
import { isConflict } from "@/api/operations";
import { MAX_NOTE } from "@/lib/text-limits";
import { submitOnEnter } from "@/lib/submit-on-enter";

// The two shapes this dialog enters, redemption and sale: an instrument, a
// quantity and incoming money, which covers every corporate event seen live. Two
// types because the journal keeps them apart. Anything else is entered on the
// account's screen and is not linked, a limit the dialog states.
const EXPLAIN_TYPES = ["redemption", "sell"] as const;
type ExplainType = (typeof EXPLAIN_TYPES)[number];

// ExplainDialog enters the one manual operation accounting for the picked
// broker rows and links them. It sends the journal's own create shape, so the
// journal's rules apply; it checks only what would make the request
// malformed.
export function ExplainDialog({
  open,
  onOpenChange,
  connectionId,
  linkId,
  contentKeys,
  onExplained,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  connectionId: string;
  // The linked broker account; the server puts the operation there.
  linkId: string;
  contentKeys: string[];
  onExplained: () => void;
}) {
  const { t } = useTranslation();
  const [type, setType] = useState<ExplainType>("redemption");
  const [instrument, setInstrument] = useState<Instrument | null>(null);
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [quantity, setQuantity] = useState("");
  const [amount, setAmount] = useState("");
  const [note, setNote] = useState("");
  const explain = useExplainRows(connectionId);

  const amountMinor = parseToMinor(amount);
  // The journal dialogs' amount rule, not a second one.
  const refusal = amountRefusal(amount);
  const ready =
    instrument !== null &&
    isPositiveDecimal(quantity) &&
    amountMinor !== null &&
    amountMinor > 0 &&
    refusal === null &&
    occurredOn !== "";

  const submit = () => {
    if (!ready || instrument === null || amountMinor === null) return;
    explain.mutate(
      {
        linkId,
        body: {
          content_keys: contentKeys,
          operation: {
            // Overwritten by the server with the linked account; sent because the
            // shape requires it.
            account_id: linkId,
            instrument_id: instrument.id,
            type,
            occurred_on: occurredOn,
            quantity,
            // Incoming money: both types refuse a non-positive amount.
            amount_minor: amountMinor,
            currency: instrument.currency,
            note,
          },
        },
      },
      {
        onSuccess: () => {
          onOpenChange(false);
          setInstrument(null);
          setQuantity("");
          setAmount("");
          setNote("");
          onExplained();
        },
      },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent onKeyDown={submitOnEnter(submit, ready && !explain.isPending)}>
        <DialogHeader>
          <DialogTitle>{t("connections.detail.explain.title")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.explain.intro", { n: contentKeys.length })}
          </p>
          <div className="grid gap-2">
            <Label htmlFor="explain-type">{t("connections.detail.explain.type")}</Label>
            <select
              id="explain-type"
              className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"
              value={type}
              onChange={(e) => setType(e.target.value as ExplainType)}
            >
              {EXPLAIN_TYPES.map((value) => (
                <option key={value} value={value}>
                  {t(`operationTypes.${value}`)}
                </option>
              ))}
            </select>
          </div>
          <div className="grid gap-2">
            <Label>{t("connections.detail.explain.instrument")}</Label>
            <InstrumentPicker value={instrument} onChange={setInstrument} />
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="explain-date">{t("connections.detail.explain.date")}</Label>
              <Input
                id="explain-date"
                type="date"
                min={EARLIEST_OPERATION_DATE}
                value={occurredOn}
                onChange={(e) => setOccurredOn(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="explain-quantity">{t("connections.detail.explain.quantity")}</Label>
              <Input
                id="explain-quantity"
                inputMode="decimal"
                value={quantity}
                onChange={(e) => setQuantity(e.target.value)}
              />
            </div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="explain-amount">
              {t("connections.detail.explain.amount", {
                currency: instrument?.currency ?? "",
              })}
            </Label>
            <Input
              id="explain-amount"
              inputMode="decimal"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="explain-note">{t("connections.detail.explain.note")}</Label>
            <Input
              id="explain-note"
              maxLength={MAX_NOTE}
              value={note}
              onChange={(e) => setNote(e.target.value)}
            />
          </div>
          {/* The hint is about which date: the two broker rows have different
             ones. */}
          <p className="text-xs text-muted-foreground">
            {t("connections.detail.explain.dateHint")}
          </p>
          {explain.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(explain.error)
                  ? t("connections.detail.explain.conflict")
                  : t("connections.detail.explain.failed")}
              </AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!ready || explain.isPending} onClick={submit}>
            {explain.isPending ? t("app.loading") : t("connections.detail.explain.submit")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
