// Tracks which distinct currencies are relevant to whatever screen is
// currently mounted inside <AppLayout>'s <Outlet/>. Two readers, one count:
// the header's display-currency toggle (DisplayCurrencyToggle) hides itself
// when there's nothing to convert — a single currency in play, with nothing
// for "native" vs "base" to ever differ on — and the screen itself decides,
// from the same count, which of the two modes its money is drawn in. They are
// one decision and must not be read from two places (see
// useScreenCurrencies).
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

// What every mounted reporter currently displays, keyed by an opaque
// per-caller id (useId) so several sections of one screen can report
// independently — see useReportScreenCurrencies.
type ReportedCurrencies = ReadonlyMap<string, readonly string[]>;

interface ScreenCurrencyActions {
  report: (reporterId: string, currencies: readonly string[]) => void;
  clear: (reporterId: string) => void;
}

// TWO contexts, not one object holding both halves, and the split is
// load-bearing rather than tidiness.
//
// The actions are what a reporter's effect depends on, so their identity must
// NEVER change: both are useCallback with no dependencies, and the value
// wrapping them is memoized on just those two, so it is created once per
// provider and never again. The reported map, in contrast, gets a fresh
// identity every time any reporter's set changes.
//
// Put the two in one object and that fresh identity reaches the effect's
// dependency list — and it does not merely churn, it never settles: a changed
// dependency makes every reporter's effect tear down and re-run, the cleanup's
// clear() followed by a fresh report() writes a new map with the same contents
// but a new identity, and that new identity changes the dependency again. The
// loop has no exit, because the value it compares is an identity that the loop
// itself replaces. Measured rather than reasoned about: with the two merged
// into one memoized object, two mounted reporters render past 500 times and
// are still going when the counter cuts them off; split as below, the same two
// render twice in total and stop.
const ScreenCurrencyActionsContext = createContext<ScreenCurrencyActions | null>(null);
const ReportedCurrenciesContext = createContext<ReportedCurrencies | null>(null);

// The distinct currencies in play, as an integer.
//
// `except` and `mine` exist for a reporter asking about ITSELF: the map holds
// whatever that reporter last reported through an effect, which on the render
// where its own set changed is a frame out of date. Passing its own id and its
// own current set replaces that stale entry with the fresh one instead of
// unioning with it — a currency the screen has stopped showing must not keep
// counting.
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

  // Created once and never again (see the note on the two contexts above):
  // both callbacks have empty dependency lists, so this memo never
  // recomputes, and a reporter's effect therefore never re-runs because of
  // the context it reads.
  const actions = useMemo(() => ({ report, clear }), [report, clear]);
  return (
    <ScreenCurrencyActionsContext.Provider value={actions}>
      <ReportedCurrenciesContext.Provider value={reported}>
        {children}
      </ReportedCurrenciesContext.Provider>
    </ScreenCurrencyActionsContext.Provider>
  );
}

// The reporting half, shared by both public hooks below: registers this
// component's currency set with the provider and withdraws it on unmount.
// Returns what a caller needs to ask the provider about itself.
function useReporter(currencies: Iterable<string>) {
  const actions = useContext(ScreenCurrencyActionsContext);
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
    if (!actions) return;
    actions.report(reporterId, list);
    return () => actions.clear(reporterId);
  }, [actions, reporterId, list]);
  return { registered: actions !== null, reporterId, list };
}

// Called by a screen — or by any self-contained section of one — with the set
// of currencies it currently displays (its rows' native currencies, plus the
// space's base currency — the toggle's conversion target — so a section whose
// rows are all a single *foreign* currency still reports 2 distinct
// currencies, not 1, and correctly shows the toggle). Several callers may be
// mounted at once; the provider counts the union of what they report. Each
// caller's contribution is withdrawn on unmount, so navigating to a screen
// that doesn't call this hook at all doesn't inherit anything left behind.
// It returns nothing: a section that reports this way draws its money in
// whatever mode its screen hands it (the journal takes `mode` as a prop from
// account detail), so that the two halves of one screen cannot disagree about
// which currency they are printing in. A SCREEN — the component that decides
// that mode — calls useScreenCurrencies below instead.
export function useReportScreenCurrencies(currencies: Iterable<string>): void {
  useReporter(currencies);
}

// Called by a SCREEN: reports its currencies exactly as above, and returns the
// display-currency mode its own money cells — and every section it hands
// `mode` to — must render with. Every screen that renders money reads the mode
// through this hook and not through useDisplayCurrency; the toggle itself is
// the one exception, since it shows the stored choice and only renders when
// visible anyway.
//
// What it returns is the EFFECTIVE mode, which is the stored one only while
// the screen has something to convert. Visibility of the header toggle and
// application of the mode are the same decision counted the same way: a screen
// with fewer than two currencies hides the toggle, so if it still *applied*
// "base" the reader would be stuck in a mode with no control left to leave it
// by — with no clue why, since the toggle is simply gone. (Concretely: turn on
// "base" somewhere multi-currency, come back to a single-currency screen, and
// the per-currency breakdown cards vanish for good, recoverable only by
// clearing localStorage.) The stored choice is deliberately left untouched:
// it is the user's, made deliberately, and it must come back by itself the
// moment a screen has something to convert again. Only its application is
// suspended.
//
// The two are one hook because the answer is needed DURING the render that
// knows the currencies. Reporting travels through an effect, which runs after
// the render commits, so a screen that read the mode back out of the provider
// spent its first frame with a count of zero: money that the user's stored
// choice says belongs in the base currency was rendered once in each row's own
// currency and then again converted (#41). Both frames were honest, and
// nothing was ever wrong for longer than a frame, but the sums visibly changed
// under a reader who had asked for one of the two.
//
// What this closes is the screen's own set, which is the whole of it on the
// accounts list and nearly all of it on account detail. A screen that is
// multi-currency ONLY because a separate section reported something (a
// dollar-denominated dividend in the journal of an all-ruble account) still
// learns that one render later, through the effect, exactly as before: a
// child's report cannot reach its parent's render, because the parent renders
// first.
export function useScreenCurrencies(currencies: Iterable<string>): DisplayCurrencyMode {
  const { mode } = useDisplayCurrency();
  const { registered, reporterId, list } = useReporter(currencies);
  const reported = useContext(ReportedCurrenciesContext);
  // Outside a provider there is no header to render the toggle, so applying
  // the stored mode would strand the reader in it — the same reason the
  // in-provider rule below exists. See useHasMultipleScreenCurrencies.
  if (!registered || !reported) return "native";
  return countDistinct(reported, reporterId, list) > 1 ? mode : "native";
}

// Read by the header: whether more than one currency is in play on the
// current screen, i.e. whether the display-currency toggle has anything
// meaningful to switch between. Defaults to false (hidden) when read
// outside a provider or before any screen has reported.
//
// This one is a commit behind a screen's own answer, and cannot help being:
// the header is not the component that knows the currencies, so what it reads
// is what the reporters' effects have delivered so far. The two agree at rest
// — a screen counts the same union, only with its own entry taken from its
// current render instead of from its last effect — so they differ only while a
// reporter's set is changing, and the screen is the one that is AHEAD.
//
// Ahead in both directions, and it is worth being exact about that rather than
// claiming the header can only ever undercount. A screen whose set has just
// widened applies the mode a commit before the toggle appears; one whose set
// has just narrowed stops applying it a commit before the toggle goes away.
// What cannot happen either way is the thing the rule exists to prevent: a
// screen left applying a mode the header has settled on giving no control for,
// since at rest the two count the same set.
export function useHasMultipleScreenCurrencies(): boolean {
  const reported = useContext(ReportedCurrenciesContext);
  return countDistinct(reported ?? NO_REPORTS) > 1;
}

