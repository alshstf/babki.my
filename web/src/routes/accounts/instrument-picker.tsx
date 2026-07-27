import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useCreateInstrument, useInstruments, type Instrument, type InstrumentType } from "@/api/instruments";

const INSTRUMENT_TYPES: InstrumentType[] = [
  "share",
  "bond",
  "etf",
  "currency",
  "crypto",
  "metal",
  "custom",
];

// InstrumentPicker: a search box + result list standing in for shadcn's
// Command/Popover (not present in this project's component set — the brief
// explicitly says not to add new deps for this). The instrument catalog is
// small (useInstruments caps at 50), so no debounce: every keystroke just
// re-runs the already-cheap local query.
export function InstrumentPicker({
  value,
  onChange,
}: {
  // Selected instrument, or null when nothing is picked yet.
  value: Instrument | null;
  onChange: (instrument: Instrument) => void;
}) {
  const { t } = useTranslation();
  const [query, setQuery] = useState("");
  const [creating, setCreating] = useState(false);
  const instruments = useInstruments(query);
  const createInstrument = useCreateInstrument();

  // New-instrument mini-form fields.
  const [newType, setNewType] = useState<InstrumentType>("share");
  const [newName, setNewName] = useState("");
  const [newTicker, setNewTicker] = useState("");
  const [newCurrency, setNewCurrency] = useState("RUB");
  const [newIsin, setNewIsin] = useState("");

  useEffect(() => {
    if (!creating) return;
    setNewType("share");
    setNewName("");
    setNewTicker("");
    setNewCurrency("RUB");
    setNewIsin("");
    createInstrument.reset();
  }, [creating]); // eslint-disable-line react-hooks/exhaustive-deps

  const newValid = newName.trim() !== "" && /^[A-Z]{3}$/.test(newCurrency.toUpperCase());

  const submitCreate = () => {
    createInstrument.mutate(
      {
        type: newType,
        name: newName,
        ticker: newTicker || undefined,
        isin: newIsin || undefined,
        currency: newCurrency.toUpperCase(),
      },
      {
        onSuccess: (instrument) => {
          onChange(instrument);
          setCreating(false);
        },
      },
    );
  };

  if (creating) {
    return (
      <div className="grid gap-3 rounded-lg border p-3">
        <div className="grid gap-2">
          <Label>{t("instrumentPicker.type")}</Label>
          <Select value={newType} onValueChange={(v) => setNewType(v as InstrumentType)}>
            <SelectTrigger><SelectValue /></SelectTrigger>
            <SelectContent>
              {INSTRUMENT_TYPES.map((instrumentType) => (
                <SelectItem key={instrumentType} value={instrumentType}>
                  {t(`instrumentTypes.${instrumentType}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="grid gap-2">
          <Label htmlFor="new-instr-name">{t("instrumentPicker.name")}</Label>
          <Input id="new-instr-name" value={newName} onChange={(e) => setNewName(e.target.value)} />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="new-instr-ticker">{t("instrumentPicker.ticker")}</Label>
          <Input id="new-instr-ticker" value={newTicker} onChange={(e) => setNewTicker(e.target.value)} />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="new-instr-currency">{t("instrumentPicker.currency")}</Label>
          <Input
            id="new-instr-currency"
            maxLength={3}
            value={newCurrency}
            onChange={(e) => setNewCurrency(e.target.value.toUpperCase())}
          />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="new-instr-isin">{t("instrumentPicker.isin")}</Label>
          <Input id="new-instr-isin" value={newIsin} onChange={(e) => setNewIsin(e.target.value)} />
        </div>
        {createInstrument.isError && (
          <Alert variant="destructive">
            <AlertDescription>{createInstrument.error.message}</AlertDescription>
          </Alert>
        )}
        <div className="flex justify-end gap-2">
          <Button variant="outline" onClick={() => setCreating(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!newValid || createInstrument.isPending} onClick={submitCreate}>
            {t("common.create")}
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="grid gap-2">
      {value ? (
        <div className="rounded-lg border bg-muted/50 px-2.5 py-1.5">
          <div className="text-sm font-medium">{value.name}</div>
          <div className="text-xs text-muted-foreground">
            {value.ticker || value.currency}
          </div>
        </div>
      ) : null}
      <Input
        placeholder={t("instrumentPicker.search")}
        value={query}
        onChange={(e) => setQuery(e.target.value)}
      />
      <div className="grid max-h-48 gap-1 overflow-y-auto">
        {instruments.isLoading ? (
          <div className="px-2 py-1.5 text-sm text-muted-foreground">{t("app.loading")}</div>
        ) : instruments.data && instruments.data.length > 0 ? (
          instruments.data.map((instrument) => (
            <button
              key={instrument.id}
              type="button"
              onClick={() => onChange(instrument)}
              className="flex items-center justify-between rounded-md px-2 py-1.5 text-left text-sm hover:bg-muted"
            >
              <span>
                {instrument.name}
                {instrument.ticker && (
                  <span className="ml-1.5 text-xs text-muted-foreground">{instrument.ticker}</span>
                )}
              </span>
              <span className="text-xs text-muted-foreground">{instrument.currency}</span>
            </button>
          ))
        ) : (
          <div className="px-2 py-1.5 text-sm text-muted-foreground">
            {t("instrumentPicker.empty")}
          </div>
        )}
      </div>
      <Button variant="outline" size="sm" onClick={() => setCreating(true)} type="button">
        {t("instrumentPicker.create")}
      </Button>
    </div>
  );
}
