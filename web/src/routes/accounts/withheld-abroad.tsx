import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useStateWithheld, type Operation } from "@/api/operations";
import { formatMinor, formatPriceIn, minorToInput, parseToMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { submitOnEnter } from "@/lib/submit-on-enter";
import { AmountField } from "@/components/form-fields";
import { Button } from "@/components/ui/button";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { useOnOpen } from "@/lib/use-on-open";

type Withheld = NonNullable<Operation["withheld_abroad"]>;

// rate is a percentage with one decimal ("30.0"), shown the Russian way.
function formatRate(rate: number | string): string {
  return new Intl.NumberFormat("ru-RU", { minimumFractionDigits: 1, maximumFractionDigits: 1 }).format(
    Number(rate),
  );
}

// The tax a foreign dividend lost abroad (Р-14), under the row's type: an
// estimate marked ≈ with its arithmetic in the tooltip, the figure a person
// stated, or «unknown» with the reason; and the tax the broker reported as its
// own row. editable offers stating the figure from the broker's statement.
export function WithheldAbroadNote({ operation, editable }: { operation: Operation; editable: boolean }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const withheld = operation.withheld_abroad as Withheld;
  const money = (minor: number, currency = withheld.currency) => formatMinor(minor, currency);
  const brokerTax = withheld.broker_tax.map((x) => money(x.amount_minor, x.currency)).join(", ");
  const inBase = withheld.tax_in_base
    ? money(withheld.tax_in_base.amount_minor, withheld.tax_in_base.currency)
    : null;

  return (
    <div className="max-w-64 text-xs whitespace-normal text-muted-foreground" data-testid="operation-withheld">
      {withheld.state === "estimated" && withheld.tax_minor != null && withheld.gross_minor != null && (
        <div
          data-testid="operation-withheld-estimate"
          title={t("operations.withheldEstimateTitle", {
            perShare: withheld.per_share ? (formatPriceIn(withheld.per_share, withheld.currency) ?? withheld.per_share) : "",
            shares: withheld.shares ? Number(withheld.shares).toLocaleString("ru-RU") : "",
            date: withheld.record_date ? formatDate(withheld.record_date) : "",
            gross: money(withheld.gross_minor),
            received: money(withheld.received_minor),
          })}
        >
          {t("operations.withheldEstimated", {
            tax: money(withheld.tax_minor),
            rate: formatRate(withheld.rate_percent ?? "0"),
          })}
          {inBase && <>, {t("operations.withheldInBase", { amount: inBase })}</>}
        </div>
      )}
      {withheld.state === "stated" && withheld.tax_minor != null && (
        <div data-testid="operation-withheld-stated">
          {t("operations.withheldStated", {
            tax: money(withheld.tax_minor),
            rate: formatRate(withheld.rate_percent ?? "0"),
          })}
          {inBase && <>, {t("operations.withheldInBaseExact", { amount: inBase })}</>}
        </div>
      )}
      {withheld.state === "unknown" && (
        <div
          data-testid="operation-withheld-unknown"
          title={withheld.unknown_reason ? t(`operations.withheldReason.${withheld.unknown_reason}`) : undefined}
        >
          {t("operations.withheldUnknown")}
        </div>
      )}
      {brokerTax !== "" && (
        <div data-testid="operation-withheld-broker">{t("operations.brokerTax", { amount: brokerTax })}</div>
      )}
      {editable && (
        <button
          type="button"
          data-testid="operation-withheld-edit"
          className="underline underline-offset-2 hover:text-foreground"
          onClick={() => setOpen(true)}
        >
          {withheld.state === "estimated" || withheld.state === "stated"
            ? t("operations.withheldCorrect")
            : t("operations.withheldState")}
        </button>
      )}
      {editable && <WithheldDialog operation={operation} open={open} onOpenChange={setOpen} />}
    </div>
  );
}

// WithheldDialog states the tax from the broker's statement, showing the rate
// it implies against what arrived; «Вернуть оценку» clears a stated one.
function WithheldDialog({
  operation,
  open,
  onOpenChange,
}: {
  operation: Operation;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const withheld = operation.withheld_abroad as Withheld;
  const save = useStateWithheld(operation);
  const [tax, setTax] = useState("");

  useOnOpen(open, () => {
    setTax(withheld.tax_minor != null ? minorToInput(withheld.tax_minor) : "");
    save.reset();
  });

  const parsed = parseToMinor(tax);
  const valid = parsed !== null && parsed >= 0;
  const gross = valid ? withheld.received_minor + parsed : null;
  const submit = (taxMinor: number | null) => save.mutate(taxMinor, { onSuccess: () => onOpenChange(false) });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md" onKeyDown={submitOnEnter(() => valid && submit(parsed), valid && !save.isPending)}>
        <DialogHeader>
          <DialogTitle>{t("operations.withheldDialog.title")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <p className="text-sm text-muted-foreground">
            {t("operations.withheldDialog.received", {
              amount: formatMinor(withheld.received_minor, withheld.currency),
            })}
          </p>
          <AmountField
            id="withheld-tax"
            label={t("operations.withheldDialog.tax", { currency: withheld.currency })}
            value={tax}
            onChange={setTax}
            currency={withheld.currency}
            accepted={valid}
            badNumber={t("operations.withheldDialog.badNumber")}
            hint={
              <p className="text-xs text-muted-foreground">
                {gross != null && gross > 0 && parsed != null
                  ? t("operations.withheldDialog.rate", {
                      rate: formatRate((parsed / gross) * 100),
                      gross: formatMinor(gross, withheld.currency),
                    })
                  : t("operations.withheldDialog.hint")}
              </p>
            }
          />
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("app.error")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          {withheld.state === "stated" && (
            <Button variant="outline" disabled={save.isPending} onClick={() => submit(null)}>
              {t("operations.withheldDialog.clear")}
            </Button>
          )}
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || save.isPending} onClick={() => valid && submit(parsed)}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
