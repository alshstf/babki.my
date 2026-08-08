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
  bondPercentFromPrice,
  bondPriceFromPercent,
  formatMinor,
  formatMinorCompact,
  multiplyToMinor,
  parseToMinor,
  isPositiveDecimal,
} from "@/lib/money";
import { localToday } from "@/lib/dates";
import { useCreateOperation, isConflict } from "@/api/operations";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";
import { InstrumentPicker } from "./instrument-picker";

// Why a bond's percentage-of-face field cannot be converted into money, or
// null when it can. Four causes, and they get four different sentences on
// screen for the reason this project keeps relearning: a caption that names
// the wrong missing thing is a wrong answer wearing the shape of a right one,
// and «номинал не записан» over a bond whose номинал is recorded in another
// currency is exactly that.
type FaceGap =
  | "no_face_value"
  | "bad_face_value"
  | "no_face_currency"
  | "face_currency_mismatch";

// faceGapOf answers, for the instrument currently picked, whether the percent
// field can do its job — and if not, which of the four things is in the way.
// Null means the conversion is available; null is also what every non-bond
// gets, because the field it describes is not rendered for them at all.
//
// The order is the order the causes stop the conversion in, so the sentence a
// reader sees is about the FIRST thing missing rather than an arbitrary one:
// with no face value at all, its currency is not the interesting news.
//
// A face value of zero or less gets a cause of its own rather than being
// folded into "not recorded": the catalog DOES hold a number for it, so a
// sentence saying nobody wrote one down would be false, and the remedy is a
// different one (fix the instrument, not merely type a price here). It cannot
// simply be multiplied either — every percentage of zero is zero, and a price
// field filled with 0,00 ₽ is the plausible-looking fabrication this project
// refuses to print.
//
// The currency comparison is against the instrument's own currency because
// that is the currency the operation is recorded in (see submit below), and
// therefore the currency the amount this dialog computes has to be in. The
// server draws the same line on the same field for the same reason: a bond's
// market value is denominated in its face currency, NOT in the quote's, and a
// face value with no currency at all buys no valuation (marketValue in
// internal/portfolio/http.go). A face value in euros cannot price a trade
// booked in rubles without an fx rate, and there is none in this dialog.
// An EMPTY face currency counts as none, and that clause is the whole reason
// this comment names it: an empty string is not null, so without it the
// instrument falls past this check into the mismatch one below and is captioned
// «Номинал в , а сделка в RUB» — a sentence naming a currency that is not there,
// which in this repository is not a typo but the defect class itself. The API
// refuses to store one now (checkFacePair in internal/instrument/http.go, and
// the CHECK constraint in migration 0012), so no server this client talks to
// should produce it — which is the same standing this file already gives a null
// face currency, and for the same reason: a client is a separate program from
// the API version in front of it.
function faceGapOf(instrument: Instrument | null): FaceGap | null {
  if (!instrument || instrument.type !== "bond") return null;
  if (instrument.face_value_minor == null) return "no_face_value";
  if (instrument.face_value_minor <= 0) return "bad_face_value";
  if (instrument.face_currency == null || instrument.face_currency === "") return "no_face_currency";
  if (instrument.face_currency !== instrument.currency) return "face_currency_mismatch";
  return null;
}

// The sentence that goes under the two price fields when the conversion is
// unavailable. Written as a switch over literal keys rather than a lookup
// table so every key stays a literal at the t() call site — the only shape
// scripts/check-i18n.mjs can verify.
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

// What the fee field says when the sum typed into it is past the bound, and
// the whole of it is WHICH CURRENCY the bound is named in (#109).
//
// The bound itself is currency-agnostic — MAX_AMOUNT_MINOR is 10^15 minor
// units of whatever the operation is denominated in, and the server refuses
// past the same figure (money.MaxAmountMinor) — so naming a currency here is a
// claim about the fee, not about the limit. The fee travels on the operation
// this dialog posts, and that operation's currency follows the INSTRUMENT
// rather than the account: a USD-denominated fund inside a RUB brokerage
// account is ordinary, and submit() sends `currency: instrument.currency`
// because a position's currency is fixed by its first operation. Formatting
// the ceiling in the account's currency therefore refused a dollar fee by
// naming a limit in rubles — the right number wearing the wrong sign, which on
// a screen about money is a different number.
//
// With no instrument picked there IS no currency yet, and the second sentence
// says so instead of guessing one. Reachable: the fee field is enabled from
// the moment the dialog opens and nothing forces the picker to come first. The
// account's currency is available and is exactly the wrong answer; a
// currency-free rendering of the number would leave the reader to guess
// whether it was units or kopecks. So the message names the number's owner
// instead — pick an instrument and the limit will be named in its currency.
//
// Two literal keys rather than one call with a computed key, so both stay
// verifiable by scripts/check-i18n.mjs.
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
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
  side: "buy" | "sell";
}) {
  const { t } = useTranslation();
  const createOperation = useCreateOperation();

  const [instrument, setInstrument] = useState<Instrument | null>(null);
  const [quantity, setQuantity] = useState("");
  // MONEY PER UNIT, always, for every instrument type — this is the one price
  // this dialog records and the one the total is struck from. A bond's
  // percentage of face is an input for it (see percentShown), never a
  // substitute: `price` goes on the wire as Operation.price, which is money
  // per unit everywhere else in this application — the journal renders a row
  // as «quantity × price» beside its amount, and a percentage there would be
  // a multiplication that visibly does not come out.
  const [price, setPrice] = useState("");
  // The percentage-of-face field WHILE THE USER IS TYPING IN IT, and null the
  // rest of the time — when it is null the field shows the percentage derived
  // from `price`, which is what keeps the pair honest with no synchronising
  // code: outside of its own edits the percent field is a pure function of
  // the money one, so the two cannot drift apart, and a change of instrument
  // (a different face value) re-answers it on its own.
  const [percentInput, setPercentInput] = useState<string | null>(null);
  const [fee, setFee] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");

  useEffect(() => {
    if (open) {
      setInstrument(null);
      setQuantity("");
      setPrice("");
      setPercentInput(null);
      setFee("");
      setOccurredOn(localToday());
      setNote("");
      createOperation.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, side]);

  const isBond = instrument?.type === "bond";
  const faceGap = faceGapOf(instrument);
  // The face value this dialog multiplies by, and null when there is none it
  // may use. No gap means faceGapOf has already established everything this
  // conversion needs: a face value is recorded, it is positive, and it is
  // denominated in the very currency the trade will be booked in.
  const faceValueMinor = isBond && faceGap === null ? (instrument.face_value_minor ?? null) : null;
  const canConvertFace = faceValueMinor !== null;

  // The percent field's displayed value. Its own draft while it is being
  // edited; otherwise whatever percentage the money price works out to.
  const percentShown =
    percentInput ?? (canConvertFace ? (bondPercentFromPrice(price, faceValueMinor) ?? "") : "");

  // The two halves of the link. Each sets its own field and re-derives the
  // other; neither is allowed to set only itself, which is the whole point of
  // showing both. Editing the money field drops the percent draft so the
  // percentage goes back to following the money.
  //
  // A percentage that does not convert — «98,5», typed with the Russian
  // decimal comma this application accepts nowhere — EMPTIES the money field
  // rather than leaving the last value that did convert standing there. That
  // costs a user his typing, and it is still the right way round: the money
  // field is the one that gets recorded, so a stale 980,00 sitting beside a
  // freshly typed 98,5 would leave the Buy button enabled over a price the
  // user can plainly see is not the one he wrote — a wrong cost basis, entered
  // in silence, which is the exact failure this pair of fields was built to
  // end. Emptied, the price is invalid, the button is disabled and
  // «Проверьте количество и цену» appears. The price is retyped; nothing wrong
  // is recorded.
  const changePercent = (value: string) => {
    setPercentInput(value);
    if (!canConvertFace) return;
    setPrice(bondPriceFromPercent(value, faceValueMinor) ?? "");
  };
  const changePrice = (value: string) => {
    setPercentInput(null);
    setPrice(value);
  };
  // A new instrument brings a new face value, so a percentage typed against
  // the old one describes nothing. Dropping the draft re-derives it from the
  // money price under the new face value instead of leaving a stale number
  // beside a live one.
  const changeInstrument = (picked: Instrument) => {
    setInstrument(picked);
    setPercentInput(null);
  };

  const qtyValid = isPositiveDecimal(quantity);
  const priceValid = isPositiveDecimal(price);
  const feeParsed = fee.trim() === "" ? 0 : parseToMinor(fee);
  const feeValid = feeParsed !== null && feeParsed >= 0;
  // Read through the same "blank means no fee" rule as feeParsed above, so the
  // field cannot complain about a fee it just accepted as zero. A fee past the
  // bound parses perfectly well, and «проверьте комиссию» would leave its author
  // checking a number that is not wrong in any way he can see (see
  // AmountRefusal).
  const feeRefusal = fee.trim() === "" ? null : amountRefusal(fee);

  // Total is computed with exact BigInt arithmetic (see multiplyToMinor) —
  // never as a float — so what's previewed here is exactly what gets sent.
  const totalMinor = qtyValid && priceValid ? multiplyToMinor(quantity, price) : null;
  const overflow = qtyValid && priceValid && totalMinor === null;

  const valid =
    instrument !== null &&
    qtyValid &&
    priceValid &&
    totalMinor !== null &&
    feeValid &&
    occurredOn !== "";

  const submit = () => {
    if (!instrument || totalMinor === null || feeParsed === null) return;
    const signedAmount = side === "buy" ? -totalMinor : totalMinor;
    createOperation.mutate(
      {
        account_id: account.id,
        instrument_id: instrument.id,
        type: side,
        occurred_on: occurredOn,
        quantity,
        price,
        amount_minor: signedAmount,
        // The operation's currency follows the instrument being traded (a
        // position's currency is fixed by its first operation and must stay
        // consistent thereafter — see internal/portfolio/engine.go), not the
        // account's own currency, which can differ (e.g. a USD-denominated
        // fund held inside a RUB brokerage account).
        currency: instrument.currency,
        fee_minor: feeParsed,
        note,
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  // A 409 from POST /api/v1/operations says the account's journal did not
  // replay with this row in it — and NOTHING FINER, which is why the sentence
  // below names no cause (#23).
  //
  // It used to say «Недостаточно бумаг на счете на эту дату», and that is false
  // twice over. The engine refuses for several unrelated reasons that all arrive
  // as this one status: a release exceeding the held quantity, an operation
  // whose currency is not the one the position's cost is already kept in, a
  // transfer whose stored FIFO breakdown no longer matches the history it is
  // replayed against, and an income total that has left the int64 range (see
  // internal/portfolio/engine.go). Worse, THE ROW REFUSED NEED NOT BE THE ROW
  // POSTED: every write replays the account's WHOLE journal, so a purchase —
  // which releases nothing and can only add to a position — comes back 409 over
  // an entry stored months earlier, on a date the client never sent. «на эту
  // дату» was a claim about the user's input in a message about someone else's
  // row. TestConflictIsNotOnlyAnOversell holds both halves of that down.
  //
  // The alternative was to make the server tell its conflicts apart, as the
  // importer does by lifting "this broker account cannot be imported" out of 409
  // into 422. It does not fit here: the split would be oversell versus
  // everything else, and "everything else" is itself a grab-bag needing a
  // cause-free sentence of its own, so one honest caption would be bought and
  // another owed. The overflow above is in neither group, and no status can
  // promise the refusal is about what the client sent.
  //
  // Same key as the cash and income dialogs, not a copy of its wording: all
  // three post to this endpoint and get the same undifferentiated status, so
  // there is one answer to keep true rather than three.
  const errorMessage = createOperation.isError
    ? isConflict(createOperation.error)
      ? t("operations.conflict")
      : t("app.error")
    : null;

  // The two fields every trade has, held as JSX values rather than as inner
  // components so a bond can lay them out differently without either input
  // being remounted (a component declared inside a render is a new type on
  // every render, and the field would lose focus on each keystroke).
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
  // The money price, and the only price this dialog records. Its label is the
  // one thing that changes for a bond: «за единицу» is what a share costs
  // apiece, «за одну облигацию» is what the percentage above works out to,
  // and next to a field showing a percentage the generic word would be the
  // ambiguity that started all this.
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
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            {side === "buy" ? t("trade.buyTitle") : t("trade.sellTitle")}
          </DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>{t("instrumentPicker.search")}</Label>
            <InstrumentPicker value={instrument} onChange={changeInstrument} />
          </div>
          {isBond && instrument ? (
            <>
              {/* The owner's own layout: the percentage first, because that is
                  the number he copies out of the terminal, then the money it
                  works out to, then the quantity. */}
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
                // The face value is spelled out rather than merely alluded to:
                // it is the number the percentage above is a percentage OF,
                // the user can check it against his broker's document, and a
                // wrong one in the catalog is otherwise invisible right up
                // until it has silently mispriced a trade.
                //
                // Formatted in the instrument's currency, which faceGapOf has
                // just proved is the face value's own — the same equality that
                // makes the conversion publishable at all.
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
          <div className="grid gap-2">
            <Label htmlFor="trade-date">{t("trade.date")}</Label>
            <Input
              id="trade-date"
              type="date"
              value={occurredOn}
              max={localToday()}
              onChange={(e) => setOccurredOn(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="trade-note">{t("trade.note")}</Label>
            <Input id="trade-note" value={note} onChange={(e) => setNote(e.target.value)} />
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
            {side === "buy" ? t("trade.buyTitle") : t("trade.sellTitle")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
