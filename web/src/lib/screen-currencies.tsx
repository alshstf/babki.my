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
import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { useDisplayCurrency, type DisplayCurrencyMode } from "./display-currency";

interface ScreenCurrencyCountContextValue {
  count: number;
  setCount: (count: number) => void;
}

const ScreenCurrencyCountContext = createContext<ScreenCurrencyCountContextValue | null>(null);

export function ScreenCurrencyCountProvider({ children }: { children: ReactNode }) {
  const [count, setCount] = useState(0);
  // Memoized so the context value is referentially stable across renders
  // where `count` didn't change. Without it every AppLayout re-render hands
  // consumers a brand-new object, and — because `ctx` is a dependency of
  // useReportScreenCurrencies' effect — that effect would tear down and
  // re-run on every such render, transiently setting the count to 0 and back.
  const value = useMemo(() => ({ count, setCount }), [count]);
  return (
    <ScreenCurrencyCountContext.Provider value={value}>
      {children}
    </ScreenCurrencyCountContext.Provider>
  );
}

// Called by a screen with the set of currencies it currently displays (its
// rows' native currencies, plus the space's base currency — the toggle's
// conversion target — so a screen whose rows are all a single *foreign*
// currency still reports 2 distinct currencies, not 1, and correctly shows
// the toggle). Reports the distinct count up to the provider, and reports 0
// back on unmount so navigating to a screen that doesn't call this hook at
// all doesn't inherit whatever count the previous screen left behind.
export function useReportScreenCurrencies(currencies: Iterable<string>): void {
  const ctx = useContext(ScreenCurrencyCountContext);
  const count = new Set(currencies).size;
  useEffect(() => {
    if (!ctx) return;
    ctx.setCount(count);
    return () => ctx.setCount(0);
  }, [ctx, count]);
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
