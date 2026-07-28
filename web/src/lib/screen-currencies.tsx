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
import { createContext, useContext, useEffect, useState, type ReactNode } from "react";

interface ScreenCurrencyCountContextValue {
  count: number;
  setCount: (count: number) => void;
}

const ScreenCurrencyCountContext = createContext<ScreenCurrencyCountContextValue | null>(null);

export function ScreenCurrencyCountProvider({ children }: { children: ReactNode }) {
  const [count, setCount] = useState(0);
  return (
    <ScreenCurrencyCountContext.Provider value={{ count, setCount }}>
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
