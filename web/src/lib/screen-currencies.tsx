// Tracks which currencies the mounted screen shows. The header's
// display-currency toggle hides itself when there is only one, and the screen
// decides from the same count which mode its money is drawn in; one decision,
// one count (see useScreenCurrencies). A context, not a module store, so the
// state resets with every screen change, including screens that never report.
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

// What every mounted reporter displays, keyed by a per-caller id, so
// sections of one screen report independently.
type ReportedCurrencies = ReadonlyMap<string, readonly string[]>;

interface ScreenCurrencyActions {
  report: (reporterId: string, currencies: readonly string[]) => void;
  clear: (reporterId: string) => void;
}

// Two contexts, not one object. The actions must keep their identity forever
// (empty-dependency callbacks, memoized once); the reported map changes on every
// report. Merged, the map's new identity re-runs every reporter's effect, whose
// clear() and report() write a new map again: an endless loop (two reporters
// rendered past 500 times when tried; split, twice).
const ScreenCurrencyActionsContext = createContext<ScreenCurrencyActions | null>(null);
const ReportedCurrenciesContext = createContext<ReportedCurrencies | null>(null);

// The distinct currencies in play. `except` and `mine` let a reporter count
// itself from its current render rather than its last effect, so a currency it
// just stopped showing does not linger.
function countDistinct(
  reported: ReportedCurrencies,
  except?: string,
  mine: readonly string[] = [],
): number {
  const union = new Set<string>(mine);
  for (const [reporterId, currencies] of reported) {
    if (reporterId === except) continue;
    for (const currency of currencies) union.add(currency);
  }
  return union.size;
}

const NO_REPORTS: ReportedCurrencies = new Map();

// Both sides are de-duplicated and sorted before reporting, so this is set
// equality, and `report` can skip an update that changes nothing.
function sameCurrencies(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((currency, i) => currency === b[i]);
}

export function ScreenCurrencyCountProvider({ children }: { children: ReactNode }) {
  // Per reporter, not one number: sections of a screen each know only their part
  // (account detail reports the account and positions, the journal its own
  // paginated query). Merging sets makes the result independent of render order
  // and survives a section unmounting.
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

  // Created once: both callbacks have empty dependencies, so reporters never
  // re-run because of this context.
  const actions = useMemo(() => ({ report, clear }), [report, clear]);
  return (
    <ScreenCurrencyActionsContext.Provider value={actions}>
      <ReportedCurrenciesContext.Provider value={reported}>
        {children}
      </ReportedCurrenciesContext.Provider>
    </ScreenCurrencyActionsContext.Provider>
  );
}

// useReporter registers this component's currencies with the provider and
// withdraws them on unmount; shared by both hooks below.
function useReporter(currencies: Iterable<string>) {
  const actions = useContext(ScreenCurrencyActionsContext);
  // This caller's id for its lifetime, so it replaces only its own report.
  const reporterId = useId();
  // De-duplicated, sorted and joined: callers build the array inline, so it
  // cannot be an effect dependency. ISO codes contain no commas.
  const key = [...new Set(currencies)].sort().join(",");
  const list = useMemo(() => (key === "" ? [] : key.split(",")), [key]);
  useEffect(() => {
    if (!actions) return;
    actions.report(reporterId, list);
    return () => actions.clear(reporterId);
  }, [actions, reporterId, list]);
  return { registered: actions !== null, reporterId, list };
}

// useReportScreenCurrencies registers the currencies a screen or section
// displays, plus the base currency, so one foreign currency still counts as two.
// The provider counts the union; a caller's part goes on unmount. It returns
// nothing: a section draws in the mode its screen hands it, so the two halves of
// a screen cannot disagree. Screens call useScreenCurrencies instead.
export function useReportScreenCurrencies(currencies: Iterable<string>): void {
  useReporter(currencies);
}

// useScreenCurrencies reports like the hook above and returns the effective
// display mode for the screen and every section it passes `mode` to. Every screen
// reads the mode here, not from useDisplayCurrency (the toggle is the
// exception).
//
// The effective mode is the stored one only while the screen has two or more
// currencies: a screen with one hides the toggle, and applying "base" there would
// strand the reader with no control to leave it. The stored choice is kept and
// returns when there is something to convert again.
//
// It is one hook because the answer is needed during the render that knows the
// currencies; reading it back after the effect gave a first frame in the wrong
// mode (#41). A screen multi-currency only through a child section's report still
// learns it a render later, since the parent renders first.
export function useScreenCurrencies(currencies: Iterable<string>): DisplayCurrencyMode {
  const { mode } = useDisplayCurrency();
  const { registered, reporterId, list } = useReporter(currencies);
  const reported = useContext(ReportedCurrenciesContext);
  // Outside a provider there is no toggle, so the stored mode is not
  // applied.
  if (!registered || !reported) return "native";
  return countDistinct(reported, reporterId, list) > 1 ? mode : "native";
}

// useHasMultipleScreenCurrencies is the header's read: more than one currency
// on the current screen; false outside a provider or before any report. It lags a
// screen's own answer by one commit while a reporter's set changes, in either
// direction; at rest the two count the same set, so a screen is never left in a
// mode the header gives no control for.
export function useHasMultipleScreenCurrencies(): boolean {
  const reported = useContext(ReportedCurrenciesContext);
  return countDistinct(reported ?? NO_REPORTS) > 1;
}

