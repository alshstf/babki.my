// Tracks how many distinct currencies are relevant to whatever screen is
// currently mounted inside <AppLayout>'s <Outlet/>, so the header's
// display-currency toggle (DisplayCurrencyToggle) can hide itself when
// there's nothing to convert — a single currency in play, with nothing for
// "native" vs "base" to ever differ on.
//
// A React context (rather than the module-level store pattern used by
// display-currency.ts) is the right fit here because this state MUST reset
// automatically whenever the mounted screen changes — including for screens
// that never call useReportScreenCurrencies at all (e.g. /family). React's
// own mount/unmount lifecycle (a useEffect cleanup, see below) is exactly
// the mechanism that guarantees that reset "for free"; a manual
// module-level store would leak the previous screen's count until some
// screen remembered to clear it, which is exactly the class of bug this
// design avoids by construction.
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { useDisplayCurrency, type DisplayCurrencyMode } from "./display-currency";

interface ScreenCurrencyCountContextValue {
  count: number;
  // Both keyed by an opaque per-caller id (useId), so that several sections
  // of one screen can report independently — see useReportScreenCurrencies.
  report: (reporterId: string, currencies: readonly string[]) => void;
  clear: (reporterId: string) => void;
}

const ScreenCurrencyCountContext = createContext<ScreenCurrencyCountContextValue | null>(null);

// Both sides come from useReportScreenCurrencies, which de-duplicates and
// sorts before reporting, so an index-wise comparison is a true set equality
// here — and it lets `report` bail out of a state update that would change
// nothing, keeping the context value referentially stable.
function sameCurrencies(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((currency, i) => currency === b[i]);
}

export function ScreenCurrencyCountProvider({ children }: { children: ReactNode }) {
  // Keyed by reporter rather than a single number: one screen can have
  // several independent sections that each know only their own part of the
  // currency picture (the account detail screen has the account + positions,
  // reported by the screen component, and the operations journal, which owns
  // its own paginated query). A single shared counter would make each
  // reporter overwrite the others, so the toggle's visibility would depend on
  // render order — flickering and lying about whether there is anything to
  // convert. Merging sets makes the result order-independent, and lets one
  // section unmount without erasing what the others reported.
  const [reported, setReported] = useState<ReadonlyMap<string, readonly string[]>>(
    () => new Map(),
  );

  const report = useCallback((reporterId: string, currencies: readonly string[]) => {
    setReported((current) => {
      const existing = current.get(reporterId);
      if (existing && sameCurrencies(existing, currencies)) return current;
      const next = new Map(current);
      next.set(reporterId, currencies);
      return next;
    });
  }, []);

  const clear = useCallback((reporterId: string) => {
    setReported((current) => {
      if (!current.has(reporterId)) return current;
      const next = new Map(current);
      next.delete(reporterId);
      return next;
    });
  }, []);

  const count = useMemo(() => {
    const union = new Set<string>();
    for (const currencies of reported.values()) {
      for (const currency of currencies) union.add(currency);
    }
    return union.size;
  }, [reported]);

  // Memoized so the context value is referentially stable across renders
  // where the union's size didn't change. Without it every AppLayout re-render
  // hands consumers a brand-new object, and — because `ctx` is a dependency of
  // useReportScreenCurrencies' effect — that effect would tear down and
  // re-run on every such render, transiently dropping the reporter's entry.
  const value = useMemo(() => ({ count, report, clear }), [count, report, clear]);
  return (
    <ScreenCurrencyCountContext.Provider value={value}>
      {children}
    </ScreenCurrencyCountContext.Provider>
  );
}

// Called by a screen — or by any self-contained section of one — with the set
// of currencies it currently displays (its rows' native currencies, plus the
// space's base currency — the toggle's conversion target — so a section whose
// rows are all a single *foreign* currency still reports 2 distinct
// currencies, not 1, and correctly shows the toggle). Several callers may be
// mounted at once; the provider counts the union of what they report. Each
// caller's contribution is withdrawn on unmount, so navigating to a screen
// that doesn't call this hook at all doesn't inherit anything left behind.
export function useReportScreenCurrencies(currencies: Iterable<string>): void {
  const ctx = useContext(ScreenCurrencyCountContext);
  // Identifies this caller for the lifetime of its component instance, so its
  // report replaces its own previous one and nobody else's.
  const reporterId = useId();
  // De-duplicated, sorted and joined into a single string: callers build the
  // array inline from query data, so it is a fresh identity on every render
  // and cannot be an effect dependency directly. ISO-4217 codes contain no
  // commas, so the join is unambiguous.
  const key = [...new Set(currencies)].sort().join(",");
  const list = useMemo(() => (key === "" ? [] : key.split(",")), [key]);
  useEffect(() => {
    if (!ctx) return;
    ctx.report(reporterId, list);
    return () => ctx.clear(reporterId);
  }, [ctx, reporterId, list]);
}

// Read by the header: whether more than one currency is in play on the
// current screen, i.e. whether the display-currency toggle has anything
// meaningful to switch between. Defaults to false (hidden) when read
// outside a provider or before any screen has reported.
export function useHasMultipleScreenCurrencies(): boolean {
  const ctx = useContext(ScreenCurrencyCountContext);
  return (ctx?.count ?? 0) > 1;
}

// The display-currency mode a screen must actually render with — as opposed
// to the mode the user has stored (useDisplayCurrency).
//
// Visibility of the header toggle and application of the mode are the same
// decision and must be read from the same place: a screen with fewer than
// two currencies hides the toggle, so if it still *applied* "base" the user
// would be stuck in a mode with no control left to leave it by — with no
// clue why, since the toggle is simply gone. (Concretely: turn on "base"
// somewhere multi-currency, come back to a single-currency screen, and the
// per-currency breakdown cards vanish for good, recoverable only by clearing
// localStorage.)
//
// The stored choice is deliberately left untouched: it's the user's, made
// deliberately, and it must come back by itself the moment a screen has
// something to convert again. Only its *application* is suspended.
//
// Every screen that renders money must read the mode through this hook, not
// through useDisplayCurrency. The toggle itself is the one exception — it
// shows the stored choice, and it only renders when visible anyway.
export function useEffectiveDisplayCurrencyMode(): DisplayCurrencyMode {
  const { mode } = useDisplayCurrency();
  const hasMultiple = useHasMultipleScreenCurrencies();
  return hasMultiple ? mode : "native";
}
