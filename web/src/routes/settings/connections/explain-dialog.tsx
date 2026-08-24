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

// The two shapes this dialog can enter. Both take an instrument, a quantity
// and money that comes IN, which is what every corporate event seen live so
// far amounts to on the owner's side — a fund's units retired for a payment, a
// holding sold. They are offered as two types rather than one because they are
// two different statements about what happened, and the journal keeps them
// apart: a redemption is the paper being retired by its issuer, a sale is the
// owner selling it.
//
// A shape this list does not cover is entered on the account's own screen and
// then... not linked — which is the honest limit of this dialog and is stated
// on it, rather than being papered over with a type picker that offers fields
// half of which are refused.
const EXPLAIN_TYPES = ["redemption", "sell"] as const;
type ExplainType = (typeof EXPLAIN_TYPES)[number];

// ExplainDialog enters the one manual operation that accounts for the broker
// rows the owner picked, and links the two.
//
// It sends the JOURNAL'S OWN create shape (see TinvestExplainRequest.operation),
// so the operation is validated and replayed by the journal's own rules —
// this dialog invents no rule of its own and checks only what it must to keep
// from sending a request it already knows is malformed.
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
  // The linked broker account whose rows these are. The server puts the
  // operation on THAT account whatever this client says, so no account is
  // asked for here.
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
  // The same refusal the journal's own dialogs show, read from the raw text
  // rather than re-derived here: what counts as an unreadable or too-large
  // amount is one rule, and this screen is not the place for a second.
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
            // The server overwrites this with the linked account's id and says
            // so in the contract. It is sent because the shape requires it, and
            // it is deliberately not something this screen tries to know.
            account_id: linkId,
            instrument_id: instrument.id,
            type,
            occurred_on: occurredOn,
            quantity,
            // Money that came IN: both types this dialog offers are refused by
            // the journal with a non-positive amount, so the sign is not a
            // choice to offer.
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
      <DialogContent>
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
              value={note}
              onChange={(e) => setNote(e.target.value)}
            />
          </div>
          {/* The date hint is about WHICH date, because the two broker rows this
              operation replaces have two different ones and neither has to be
              the day the owner means. */}
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
