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

  // Has THIS query answered? `data !== undefined` is not that question:
  // useInstruments carries the previous key's rows over to the new key
  // (placeholderData: keepPreviousData), so after any answer at all the next
  // keystroke starts with rows already in hand and react-query only says so
  // through isPlaceholderData. Both verdicts below — «ничего не найдено» and
  // the offline notice — are claims about the text in the box right now, so
  // they are decided by this and not by whether there is anything to show.
  const answered = !instruments.isPlaceholderData && instruments.data !== undefined;
  // Rows carried over from the previous query stay on screen (that is what
  // keepPreviousData is for), but they never decide a verdict: they are real
  // instruments, correctly named, and picking one is right whatever query
  // fetched them.
  const rows = instruments.data ?? [];

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
            <AlertDescription>{t("app.error")}</AlertDescription>
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
      {/* A search that did not answer, said outside the scrolling list on
          purpose: the warning it carries — do not create what may already be
          there — is worth nothing if it can be scrolled out of sight. */}
      {instruments.isError && (
        <Alert variant="destructive">
          <AlertDescription className="grid gap-2">
            <span>{t("instrumentPicker.searchFailed")}</span>
            <Button
              variant="outline"
              size="sm"
              type="button"
              onClick={() => void instruments.refetch()}
            >
              {t("common.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      )}
      <div className="grid max-h-48 gap-1 overflow-y-auto">
        {instruments.isError ? null : !answered && instruments.fetchStatus === "paused" ? (
          // The request has not been sent: "paused" is react-query holding it
          // because the browser reports itself offline (networkMode "online",
          // the default), and it will go out on its own once the connection
          // returns. Said even when rows from the previous query are in hand:
          // those rows answer a question the reader has already typed past, and
          // deciding this branch on `data` alone is exactly how «Ничего не
          // найдено» reached the screen for a request nobody had sent (#88).
          // Not keyed on isLoading either, which is isPending && isFetching and
          // is therefore FALSE while paused.
          <div className="px-2 py-1.5 text-sm text-muted-foreground">
            {t("instrumentPicker.searchOffline")}
          </div>
        ) : rows.length > 0 ? (
          rows.map((instrument) => (
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
        ) : answered ? (
          // Nothing found — and this is the one caption the reader acts on:
          // what follows it is «Создать инструмент», and the duplicate it makes
          // is something this application can neither merge nor delete. So it
          // is said only about a query that came back empty itself.
          <div className="px-2 py-1.5 text-sm text-muted-foreground">
            {t("instrumentPicker.empty")}
          </div>
        ) : (
          <div className="px-2 py-1.5 text-sm text-muted-foreground">{t("app.loading")}</div>
        )}
      </div>
      <Button variant="outline" size="sm" onClick={() => setCreating(true)} type="button">
        {t("instrumentPicker.create")}
      </Button>
    </div>
  );
}
