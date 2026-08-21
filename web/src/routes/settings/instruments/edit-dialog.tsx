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
import { Checkbox } from "@/components/ui/checkbox";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useUpdateInstrument, type Instrument } from "@/api/instruments";

// The fields this dialog edits, in the order they are shown. The face value
// pair (face_value_minor / face_currency) is deliberately NOT here: its rule is
// "send both or neither, and only a bond may carry one", which is a form of its
// own rather than two more inputs, and no paper in the owner's catalog has a
// wrong one. Nothing here silently drops it — an omitted field leaves the
// stored value exactly as it stands (see UpdateInstrumentRequest).
const TEXT_FIELDS = ["name", "ticker", "isin", "figi"] as const;
type TextField = (typeof TEXT_FIELDS)[number];

export function InstrumentEditDialog({
  instrument,
  onOpenChange,
}: {
  instrument: Instrument | undefined;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const update = useUpdateInstrument();
  const [values, setValues] = useState<Record<TextField, string>>({
    name: "",
    ticker: "",
    isin: "",
    figi: "",
  });
  const [frozen, setFrozen] = useState(false);

  // Reloaded from the row every time the dialog opens on one, so a form left
  // half-typed on one paper cannot reappear over another.
  useEffect(() => {
    if (!instrument) return;
    setValues({
      name: instrument.name,
      ticker: instrument.ticker,
      isin: instrument.isin,
      figi: instrument.figi,
    });
    setFrozen(instrument.frozen);
    update.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [instrument?.id]);

  if (!instrument) return null;

  // ONLY WHAT CHANGED IS SENT. A PATCH that carried every field would rewrite
  // values nobody touched — harmless while this form holds all of them, and a
  // silent overwrite the moment it does not (the face value pair is not on this
  // form at all, and neither is anything a later version adds).
  const changed: Record<string, unknown> = {};
  for (const field of TEXT_FIELDS) {
    if (values[field] !== instrument[field]) changed[field] = values[field];
  }
  if (frozen !== instrument.frozen) changed.frozen = frozen;

  // The name is the one field the server refuses empty. Checked here so the
  // reader is told at the field rather than by a save that fails.
  const emptyName = values.name.trim() === "";
  const nothingToSave = Object.keys(changed).length === 0;

  return (
    <Dialog open={instrument !== undefined} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>{t("instruments.edit.title")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          {TEXT_FIELDS.map((field) => (
            <div className="grid gap-2" key={field}>
              <Label htmlFor={`instrument-${field}`}>
                {t(`instruments.fields.${field}`)}
              </Label>
              <Input
                id={`instrument-${field}`}
                data-testid={`instrument-${field}`}
                value={values[field]}
                onChange={(e) =>
                  setValues((v) => ({ ...v, [field]: e.target.value }))
                }
              />
              {field === "isin" && (
                <p className="text-xs text-muted-foreground">
                  {t("instruments.edit.isinHint")}
                </p>
              )}
            </div>
          ))}
          {emptyName && (
            <p
              className="text-xs text-red-500"
              data-testid="instrument-name-empty"
            >
              {t("instruments.edit.nameRequired")}
            </p>
          )}
          <label className="flex items-center gap-2 text-sm">
            <Checkbox
              checked={frozen}
              data-testid="instrument-frozen"
              onCheckedChange={(v) => setFrozen(v === true)}
            />
            {t("instruments.fields.frozen")}
          </label>
          <p className="text-xs text-muted-foreground">
            {t("instruments.edit.frozenHint")}
          </p>
          {/* What the client knows about a refusal is that this save did not
              happen. WHY is the server's own business: its message is English
              prose written for a log and is not part of the contract. The two
              causes a reader could act on — an empty name, and nothing having
              changed — are answered above without sending anything. */}
          {update.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {t("instruments.edit.saveError")}
              </AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button
            data-testid="instrument-save"
            disabled={emptyName || nothingToSave || update.isPending}
            onClick={() =>
              update.mutate(
                { id: instrument.id, body: changed },
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
