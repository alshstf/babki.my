import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { MAX_AMOUNT_MINOR, amountRefusal, formatMinorCompact } from "@/lib/money";
import { EARLIEST_OPERATION_DATE, localToday } from "@/lib/dates";

// AmountField is a sum of money typed by hand: its label, the box, an optional
// hint, and — once something is typed the caller will not take — why. A number
// larger than any sum this program holds is said as such; every other refusal
// in the caller's own words (badNumber). What the box holds and whether it is
// acceptable stay the caller's: a balance may be negative, a payment may not.
export function AmountField({
  id,
  label,
  value,
  onChange,
  currency,
  accepted,
  badNumber,
  hint,
  placeholder = "0",
}: {
  id: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  currency: string;
  accepted: boolean;
  badNumber: string;
  hint?: ReactNode;
  placeholder?: string;
}) {
  const { t } = useTranslation();
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        inputMode="decimal"
        placeholder={placeholder}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
      {hint}
      {value !== "" && !accepted && (
        <p className="text-xs text-red-500">
          {amountRefusal(value) === "tooLarge"
            ? t("common.amountTooLarge", { max: formatMinorCompact(MAX_AMOUNT_MINOR, currency) })
            : badNumber}
        </p>
      )}
    </div>
  );
}

// OperationDateField is the day an operation happened, offered within the
// range the journal accepts it in: not before its first day, not after today.
export function OperationDateField({
  id,
  label,
  value,
  onChange,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
}) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        type="date"
        value={value}
        min={EARLIEST_OPERATION_DATE}
        max={localToday()}
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  );
}
