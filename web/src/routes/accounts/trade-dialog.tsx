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
import { OperationDateField } from "@/components/form-fields";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  MAX_AMOUNT_MINOR,
  amountRefusal,
  bondPercentFromPrice,
  bondPriceFromPercent,
  formatMinor,
  formatMinorCompact,
  minorToInput,
  multiplyToMinor,
  parseToMinor,
  isPositiveDecimal,
} from "@/lib/money";
import { localToday } from "@/lib/dates";
import { useSaveOperation, isConflict, type Operation } from "@/api/operations";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";
import { InstrumentPicker } from "./instrument-picker";
import { KindMismatch } from "./kind-mismatch";
import { fitsKind } from "@/lib/operation-kinds";
import { MAX_NOTE } from "@/lib/text-limits";
import { submitOnEnter } from "@/lib/submit-on-enter";

// Why a bond's percentage-of-face field cannot convert into money, or null.
// Four causes, four sentences: «номинал не записан» over a face recorded in
// another currency would name the wrong missing thing.
type FaceGap =
  | "no_face_value"
  | "bad_face_value"
  | "no_face_currency"
  | "face_currency_mismatch";

// faceGapOf says whether the percent field can convert for the picked
// instrument, and if not, the first thing in the way. Null means it can, and for
// every non-bond (the field is not shown). A face of zero or less is its own
// cause: a number is recorded, and every percentage of zero is a fabricated 0,00
// ₽. The face currency must equal the instrument's, the currency the trade is
// booked in, since the dialog has no fx rate (the server values a bond in its
// face currency too, see marketValue). An empty face currency counts as none, or
// the mismatch sentence would read «Номинал в , а сделка в RUB»; the API refuses
// one now (checkFacePair, migration 0012), but a client may face an older
// server.
function faceGapOf(instrument: Instrument | null): FaceGap | null {
  if (!instrument || instrument.type !== "bond") return null;
  if (instrument.face_value_minor == null) return "no_face_value";
  if (instrument.face_value_minor <= 0) return "bad_face_value";
  if (instrument.face_currency == null || instrument.face_currency === "") return "no_face_currency";
  if (instrument.face_currency !== instrument.currency) return "face_currency_mismatch";
  return null;
}

// The sentence under the price fields when conversion is unavailable; literal
// t() keys for scripts/check-i18n.mjs.
function faceGapMessage(
  t: (key: string, opts?: Record<string, string>) => string,
  gap: FaceGap,
  instrument: Instrument,
): string {
  switch (gap) {
    case "no_face_value":
      return t("trade.bondNoFaceValue");
    case "bad_face_value":
      return t("trade.bondBadFaceValue");
    case "no_face_currency":
      return t("trade.bondNoFaceCurrency");
    case "face_currency_mismatch":
      return t("trade.bondFaceCurrencyMismatch", {
        face: instrument.face_currency ?? "",
        trade: instrument.currency,
      });
  }
}

// The fee field's message past the bound, named in the instrument's currency
// (#109): the operation is in the instrument's currency, not the account's (a USD
// fund in a RUB account), so a limit in roubles over a dollar fee would be the
// wrong sign. With no instrument picked there is no currency yet, and the message
// says to pick one. Two literal keys for scripts/check-i18n.mjs.
function feeCeilingMessage(
  t: (key: string, opts?: Record<string, string>) => string,
  instrument: Instrument | null,
): string {
  if (!instrument) return t("trade.feeTooLargeNoInstrument");
  return t("common.amountTooLarge", {
    max: formatMinorCompact(MAX_AMOUNT_MINOR, instrument.currency),
  });
}

export function TradeDialog({
  open,
  onOpenChange,
  account,
  side,
  editing,
  editingInstrument,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
  side: "buy" | "sell";
  // The recorded trade being corrected in place, and its paper.
  editing?: Operation;
  editingInstrument?: Instrument | null;
}) {
  const { t } = useTranslation();
  const createOperation = useSaveOperation(editing?.id);

  const [instrument, setInstrument] = useState<Instrument | null>(null);
  const [quantity, setQuantity] = useState("");
  // Money per unit for every type: the one price recorded and the one the total
  // uses. A bond's percentage is an input to it (see percentShown), never a
  // substitute: the journal shows «quantity × price» beside the amount.
  const [price, setPrice] = useState("");
  // The percent field's text while the user types in it, null otherwise; then
  // the field shows the percentage derived from `price`, so the two cannot drift
  // and a new face value re-derives it.
  const [percentInput, setPercentInput] = useState<string | null>(null);
  const [fee, setFee] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");

  useEffect(() => {
    if (open) {
      setInstrument(editingInstrument ?? null);
      setQuantity(editing?.quantity ?? "");
      setPrice(editing?.price ?? "");
      setPercentInput(null);
      setFee(editing && editing.fee_minor !== 0 ? minorToInput(editing.fee_minor) : "");
      setOccurredOn(editing?.occurred_on ?? localToday());
      setNote(editing?.note ?? "");
      createOperation.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, side]);

  const isBond = instrument?.type === "bond";
  const faceGap = faceGapOf(instrument);
  // The face value used for conversion, or null; no gap means it is recorded,
  // positive and in the trade's currency.
  const faceValueMinor = isBond && faceGap === null ? (instrument.face_value_minor ?? null) : null;
  const canConvertFace = faceValueMinor !== null;

  // The percent field's value: its draft while edited, else derived from the
  // money price.
  const percentShown =
    percentInput ?? (canConvertFace ? (bondPercentFromPrice(price, faceValueMinor) ?? "") : "");

  // Each field sets itself and re-derives the other; editing money drops the
  // percent draft. A percentage that does not convert («98,5», with the comma
  // this application does not accept) empties the money field rather than leaving
  // a stale price under the Buy button: the price becomes invalid and the button is
  // disabled.
  const changePercent = (value: string) => {
    setPercentInput(value);
    if (!canConvertFace) return;
    setPrice(bondPriceFromPercent(value, faceValueMinor) ?? "");
  };
  const changePrice = (value: string) => {
    setPercentInput(null);
    setPrice(value);
  };
  // A new instrument brings a new face value, so the percent draft is
  // dropped and re-derived.
  const changeInstrument = (picked: Instrument) => {
    setInstrument(picked);
    setPercentInput(null);
  };

  const qtyValid = isPositiveDecimal(quantity);
  const priceValid = isPositiveDecimal(price);
  const feeParsed = fee.trim() === "" ? 0 : parseToMinor(fee);
  const feeValid = feeParsed !== null && feeParsed >= 0;
  // Blank means no fee, as in feeParsed, so the field never complains about a
  // fee it accepted; a fee past the bound is reported as such, not as malformed
  // (see AmountRefusal).
  const feeRefusal = fee.trim() === "" ? null : amountRefusal(fee);

  // A preview only: the server works the amount out from quantity and price in
  // its own rounding (CreateOperationRequest.amount_minor). Both are held to one
  // table (src/lib/testdata/trade-amounts.json).
  const totalMinor = qtyValid && priceValid ? multiplyToMinor(quantity, price) : null;
  const overflow = qtyValid && priceValid && totalMinor === null;

  const kindOk = instrument === null || fitsKind(side, instrument.type);
  const valid =
    instrument !== null &&
    kindOk &&
    qtyValid &&
    priceValid &&
    totalMinor !== null &&
    feeValid &&
    occurredOn !== "";

  const submit = () => {
    if (!instrument || totalMinor === null || feeParsed === null) return;
    createOperation.mutate(
      {
        account_id: account.id,
        instrument_id: instrument.id,
        type: side,
        occurred_on: occurredOn,
        quantity,
        price,
        // The operation's currency follows the instrument (a position's currency is
        // fixed by its first operation), not the account's.
        currency: instrument.currency,
        fee_minor: feeParsed,
        note,
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  // A 409 says only that the account's journal did not replay with this row
  // (#23), so the sentence names no cause: the engine refuses for several reasons,
  // and every write replays the whole journal, so the refused row may be one stored
  // months earlier (TestConflictIsNotOnlyAnOversell). Splitting the status would buy
  // one honest caption and owe another. The same key as the cash and income
  // dialogs, which post to the same endpoint.
  const errorMessage = createOperation.isError
    ? isConflict(createOperation.error)
      ? t("operations.conflict")
      : t("app.error")
    : null;

  // The two fields every trade has, as JSX values rather than inner components,
  // so a bond can lay them out differently without remounting an input (and losing
  // focus on each keystroke).
  const quantityField = (
    <div className="grid gap-2">
      <Label htmlFor="trade-qty">{t("trade.quantity")}</Label>
      <Input
        id="trade-qty"
        inputMode="decimal"
        value={quantity}
        onChange={(e) => setQuantity(e.target.value)}
      />
    </div>
  );
  // The money price, the only one recorded. For a bond its label says «за одну
  // облигацию», what the percentage works out to.
  const priceField = (
    <div className="grid gap-2">
      <Label htmlFor="trade-price">
        {isBond ? t("trade.pricePerBond") : t("trade.price")}
        {instrument && ` (${instrument.currency})`}
      </Label>
      <Input
        id="trade-price"
        inputMode="decimal"
        value={price}
        onChange={(e) => changePrice(e.target.value)}
      />
    </div>
  );

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md" onKeyDown={submitOnEnter(submit, valid && !createOperation.isPending)}>
        <DialogHeader>
          <DialogTitle>
            {editing
              ? t("operations.editTitle")
              : side === "buy"
                ? t("trade.buyTitle")
                : t("trade.sellTitle")}
          </DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>{t("instrumentPicker.search")}</Label>
            <InstrumentPicker value={instrument} onChange={changeInstrument} />
            {instrument && !kindOk && <KindMismatch type={side} kind={instrument.type} />}
          </div>
          {isBond && instrument ? (
            <>
              {/* The owner's order: the percentage first (what the terminal shows),
                 then the money, then the quantity. */}
              <div className="grid grid-cols-2 gap-4">
                <div className="grid gap-2">
                  <Label htmlFor="trade-price-percent">{t("trade.pricePercentOfFace")}</Label>
                  <Input
                    id="trade-price-percent"
                    inputMode="decimal"
                    value={percentShown}
                    disabled={!canConvertFace}
                    onChange={(e) => changePercent(e.target.value)}
                  />
                </div>
                {priceField}
              </div>
              {faceGap !== null ? (
                <p data-testid="trade-bond-gap" className="text-xs text-muted-foreground">
                  {faceGapMessage(t, faceGap, instrument)}
                </p>
              ) : faceValueMinor !== null ? (
                // The face value is spelled out: it is what the percentage is of, and a
                // wrong one in the catalog would otherwise misprice a trade unseen. In the
                // instrument's currency, which faceGapOf proved is the face's own.
                <p data-testid="trade-bond-hint" className="text-xs text-muted-foreground">
                  {t("trade.bondPriceHint", {
                    face: formatMinor(faceValueMinor, instrument.currency),
                  })}
                </p>
              ) : null}
              {quantityField}
            </>
          ) : (
            <div className="grid grid-cols-2 gap-4">
              {quantityField}
              {priceField}
            </div>
          )}
          {(quantity !== "" || price !== "" || percentShown !== "") &&
            !overflow &&
            (!qtyValid || !priceValid) && (
              <p className="text-xs text-red-500">{t("trade.badNumber")}</p>
            )}
          {overflow && <p className="text-xs text-red-500">{t("trade.badNumber")}</p>}
          <div className="grid gap-2">
            <Label htmlFor="trade-fee">{t("trade.fee")}</Label>
            <Input
              id="trade-fee"
              inputMode="decimal"
              placeholder="0"
              value={fee}
              onChange={(e) => setFee(e.target.value)}
            />
            {fee !== "" && !feeValid && (
              <p className="text-xs text-red-500">
                {feeRefusal === "tooLarge"
                  ? feeCeilingMessage(t, instrument)
                  : t("trade.badFee")}
              </p>
            )}
          </div>
          <OperationDateField id="trade-date" label={t("trade.date")} value={occurredOn} onChange={setOccurredOn} />
          <div className="grid gap-2">
            <Label htmlFor="trade-note">{t("trade.note")}</Label>
            <Input id="trade-note" maxLength={MAX_NOTE} value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
          {totalMinor !== null && instrument && (
            <div
              data-testid="trade-total"
              className="rounded-lg border bg-muted/50 px-2.5 py-2 text-sm"
            >
              <span className="text-muted-foreground">{t("trade.total")}: </span>
              <span className="font-medium tabular-nums">
                {formatMinor(totalMinor, instrument.currency)}
              </span>
            </div>
          )}
          {errorMessage && (
            <Alert variant="destructive">
              <AlertDescription>{errorMessage}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || createOperation.isPending} onClick={submit}>
            {editing
              ? t("common.save")
              : side === "buy"
                ? t("trade.buyTitle")
                : t("trade.sellTitle")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
