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
import { AmountField, OperationDateField } from "@/components/form-fields";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  formatMinor,
  minorToInput,
  parseToMinor,
} from "@/lib/money";
import { localToday } from "@/lib/dates";
import {
  useSaveOperation,
  isConflict,
  type Operation,
  type OperationType,
} from "@/api/operations";
import type { AccountWithBalance } from "@/api/accounts";
import type { Instrument } from "@/api/instruments";
import { InstrumentPicker } from "./instrument-picker";
import { KindMismatch } from "./kind-mismatch";
import { fitsKind } from "@/lib/operation-kinds";
import { MAX_NOTE } from "@/lib/text-limits";
import { submitOnEnter } from "@/lib/submit-on-enter";
import { useOnOpen } from "@/lib/use-on-open";

// Dividend and coupon may be recorded at the cash level (no instrument) per
// the backend's validation contract (Type.RequiresInstrument in
// internal/portfolio/operation.go); amortization always needs one.
const INCOME_TYPES: OperationType[] = ["dividend", "coupon", "amortization"];
const REQUIRES_INSTRUMENT = new Set<OperationType>(["amortization"]);

// Amortization is the one type here that is not income (#109): the engine
// books it as a disposal of basis, never as income, so it never reaches «Доход».
// The title says «Выплата», true of all three, and a note says where an
// amortization goes without promising a figure.
const NOT_INCOME = new Set<OperationType>(["amortization"]);

export function IncomeDialog({
  open,
  onOpenChange,
  account,
  editing,
  editingInstrument,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
  // The recorded operation this dialog was opened on, to be corrected in
  // place, and the paper it names (null for a payment on the account itself).
  editing?: Operation;
  editingInstrument?: Instrument | null;
}) {
  const { t } = useTranslation();
  const createOperation = useSaveOperation(editing?.id);

  const [type, setType] = useState<OperationType>("dividend");
  const [instrument, setInstrument] = useState<Instrument | null>(null);
  const [amount, setAmount] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");
  // The bond's face value per unit before an amortization (Р-4): optional, so
  // an empty field is valid and sends nothing.
  const [faceBefore, setFaceBefore] = useState("");

  useOnOpen(open, () => {
    setType(editing?.type ?? "dividend");
    setInstrument(editingInstrument ?? null);
    setAmount(editing ? minorToInput(editing.amount_minor) : "");
    setOccurredOn(editing?.occurred_on ?? localToday());
    setNote(editing?.note ?? "");
    setFaceBefore(editing?.face_before_minor != null ? minorToInput(editing.face_before_minor) : "");
    createOperation.reset();
  });

  const parsed = parseToMinor(amount);
  const amountValid = parsed !== null && parsed > 0;
  const instrumentOk = instrument !== null || !REQUIRES_INSTRUMENT.has(type);
  const kindOk = instrument === null || fitsKind(type, instrument.type);
  const asksFace = type === "amortization";
  const currency = instrument ? instrument.currency : account.currency;
  const faceParsed = parseToMinor(faceBefore);
  const faceValid = !asksFace || faceBefore === "" || (faceParsed !== null && faceParsed > 0);
  const valid = amountValid && instrumentOk && kindOk && faceValid && occurredOn !== "";
  const catalogFace =
    instrument?.face_value_minor != null && instrument.face_currency === currency
      ? formatMinor(instrument.face_value_minor, currency)
      : null;

  const submit = () => {
    if (!valid || parsed === null) return;
    createOperation.mutate(
      {
        account_id: account.id,
        instrument_id: instrument?.id,
        type,
        occurred_on: occurredOn,
        amount_minor: parsed,
        // Follows the instrument's currency when one is attributed; falls
        // back to the account's own currency for a cash-level entry.
        currency,
        note,
        ...(asksFace && faceParsed !== null && faceBefore !== "" ? { face_before_minor: faceParsed } : {}),
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md" onKeyDown={submitOnEnter(submit, valid && !createOperation.isPending)}>
        <DialogHeader>
          <DialogTitle>{editing ? t("operations.editTitle") : t("income.title")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="income-type">{t("income.type")}</Label>
            <Select
              value={type}
              onValueChange={(v) => setType(v as OperationType)}
              disabled={editing !== undefined}
            >
              <SelectTrigger id="income-type"><SelectValue /></SelectTrigger>
              <SelectContent>
                {INCOME_TYPES.map((incomeType) => (
                  <SelectItem key={incomeType} value={incomeType}>
                    {t(`operationTypes.${incomeType}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {NOT_INCOME.has(type) && (
              <p
                data-testid="income-amortization-note"
                className="text-xs text-muted-foreground"
              >
                {t("income.amortizationNote")}
              </p>
            )}
          </div>
          <div className="grid gap-2">
            <div className="flex items-center justify-between">
              <Label>
                {REQUIRES_INSTRUMENT.has(type)
                  ? t("instrumentPicker.search")
                  : t("income.instrumentOptional")}
              </Label>
              {instrument && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => setInstrument(null)}
                >
                  {t("income.clearInstrument")}
                </Button>
              )}
            </div>
            <InstrumentPicker value={instrument} onChange={setInstrument} />
            {!instrumentOk && (
              <p className="text-xs text-red-500">{t("income.instrumentRequired")}</p>
            )}
            {instrument && !kindOk && <KindMismatch type={type} kind={instrument.type} />}
          </div>
          <AmountField
            id="income-amount"
            label={t("income.amount", { currency })}
            value={amount}
            onChange={setAmount}
            currency={currency}
            accepted={amountValid}
            badNumber={t("income.badNumber")}
          />
          {asksFace && (
            <AmountField
              id="income-face-before"
              label={t("income.faceBefore", { currency })}
              value={faceBefore}
              onChange={setFaceBefore}
              currency={currency}
              accepted={faceValid}
              badNumber={t("income.badFace")}
              hint={
                <>
                  <p data-testid="income-face-hint" className="text-xs text-muted-foreground">
                    {t("income.faceBeforeHint")}
                  </p>
                  {catalogFace && (
                    <p data-testid="income-face-catalog" className="text-xs text-muted-foreground">
                      {t("income.faceInCatalog", { face: catalogFace })}
                    </p>
                  )}
                </>
              }
            />
          )}
          <OperationDateField id="income-date" label={t("income.date")} value={occurredOn} onChange={setOccurredOn} />
          <div className="grid gap-2">
            <Label htmlFor="income-note">{t("income.note")}</Label>
            <Input id="income-note" maxLength={MAX_NOTE} value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
          {createOperation.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(createOperation.error) ? t("operations.conflict") : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || createOperation.isPending} onClick={submit}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
