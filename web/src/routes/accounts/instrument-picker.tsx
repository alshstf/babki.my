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
import {
  instrumentsOf,
  useCreateInstrument,
  useInstruments,
  type Instrument,
  type InstrumentType,
} from "@/api/instruments";
import { MAX_INSTRUMENT_NAME, MAX_TICKER } from "@/lib/text-limits";

const INSTRUMENT_TYPES: InstrumentType[] = [
  "share",
  "bond",
  "etf",
  "currency",
  "crypto",
  "metal",
  "custom",
];

// InstrumentPicker: a search box and result list (no Command/Popover in this
// project's components). No debounce: each keystroke reads one page. It is the
// only place the catalog is shown as a list, used by the trade, income and
// transfer dialogs, so «показать ещё» lives here (#104): with an empty box the
// list is the catalog, and a list that stopped at fifty invited duplicates that
// cannot be merged or deleted.
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

  // Has this query answered? Rows from the previous key stay as placeholder
  // data, so the two verdicts below (nothing found, offline) use
  // isPlaceholderData rather than `data`.
  const answered = !instruments.isPlaceholderData && instruments.data !== undefined;
  // Carried-over rows stay visible and pickable; they never decide a
  // verdict.
  const rows = instrumentsOf(instruments.data);
  // The server's answer, not the page length (#86). While placeholder rows
  // are shown, react-query reports no next page.
  const canLoadMore = instruments.hasNextPage;

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
      <div className="grid gap-3 rounded-lg border p-3" data-enter-ignore>
        <div className="grid gap-2">
          <Label htmlFor="new-instr-type">{t("instrumentPicker.type")}</Label>
          <Select value={newType} onValueChange={(v) => setNewType(v as InstrumentType)}>
            <SelectTrigger id="new-instr-type"><SelectValue /></SelectTrigger>
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
          <Input id="new-instr-name" maxLength={MAX_INSTRUMENT_NAME} value={newName} onChange={(e) => setNewName(e.target.value)} />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="new-instr-ticker">{t("instrumentPicker.ticker")}</Label>
          <Input id="new-instr-ticker" maxLength={MAX_TICKER} value={newTicker} onChange={(e) => setNewTicker(e.target.value)} />
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
        data-enter-ignore
        aria-label={t("instrumentPicker.search")}
        placeholder={t("instrumentPicker.search")}
        value={query}
        onChange={(e) => setQuery(e.target.value)}
      />
      {/* Outside the scrolling list: a warning that can scroll away warns
         nobody. */}
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
          // The request is paused (offline); said even with old rows in hand,
          // which answer a query already typed past (#88). isLoading is false while
          // paused.
          <div className="px-2 py-1.5 text-sm text-muted-foreground">
            {t("instrumentPicker.searchOffline")}
          </div>
        ) : rows.length > 0 ? (
          <>
            {rows.map((instrument) => (
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
            ))}
            {/* At the end of the rows, where «это всё?» comes up. */}
            {canLoadMore && (
              <Button
                variant="outline"
                size="sm"
                type="button"
                disabled={instruments.isFetchingNextPage}
                onClick={() => void instruments.fetchNextPage()}
              >
                {instruments.isFetchingNextPage ? t("app.loading") : t("instrumentPicker.loadMore")}
              </Button>
            )}
          </>
        ) : answered ? (
          // Nothing found: said only about a query that answered empty, since
          // «Создать инструмент» follows.
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
