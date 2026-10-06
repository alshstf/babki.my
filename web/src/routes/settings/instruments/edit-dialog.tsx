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
import { submitOnEnter } from "@/lib/submit-on-enter";

// The fields edited, in order. The face value pair is not here: "both or
// neither, bonds only" is a form of its own, and an omitted field is left as
// stored.
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
  const [coin, setCoin] = useState("");

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
    setCoin(instrument.coingecko_id ?? "");
    update.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [instrument?.id]);

  if (!instrument) return null;

  // Only changed fields are sent, so untouched or absent fields are never
  // overwritten.
  const changed: Record<string, unknown> = {};
  for (const field of TEXT_FIELDS) {
    if (values[field] !== instrument[field]) changed[field] = values[field];
  }
  if (frozen !== instrument.frozen) changed.frozen = frozen;
  if (instrument.type === "crypto" && coin.trim().toLowerCase() !== (instrument.coingecko_id ?? "")) {
    changed.coingecko_id = coin;
  }

  // The name is the one field the server refuses empty. Checked here so the
  // reader is told at the field rather than by a save that fails.
  const emptyName = values.name.trim() === "";
  const nothingToSave = Object.keys(changed).length === 0;

  const canSave = !emptyName && !nothingToSave && !update.isPending;
  const save = () => update.mutate({ id: instrument.id, body: changed }, { onSuccess: () => onOpenChange(false) });

  return (
    <Dialog open={instrument !== undefined} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm" onKeyDown={submitOnEnter(save, canSave)}>
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
          {/* Decision Р-20: the coin CoinGecko prices this cryptocurrency as. */}
          {instrument.type === "crypto" && (
            <div className="grid gap-2">
              <Label htmlFor="instrument-coin">{t("instruments.fields.coingeckoId")}</Label>
              <Input
                id="instrument-coin"
                data-testid="instrument-coin"
                value={coin}
                onChange={(e) => setCoin(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">{t("instruments.edit.coingeckoIdHint")}</p>
            </div>
          )}
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
          {/* The client knows only that the save did not happen; the server's
             English is not the contract. An empty name and no changes are caught
             before sending. */}
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
            disabled={!canSave}
            onClick={save}
          >
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
