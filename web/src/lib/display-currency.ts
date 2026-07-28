// Display currency mode: whether money amounts on screen show in each
// account/position's own ("native") currency or converted into the space's
// base currency (using the backend's `in_base` figures). This is a
// per-browser display preference, not app data, so it lives in localStorage
// rather than on the backend.
//
// A tiny module-level store (not a dependency) backs `useSyncExternalStore`
// so every component using `useDisplayCurrency()` re-renders together when
// the mode changes — whether the change came from another component in this
// tab (plain function call) or from another tab (the `storage` event, which
// browsers only ever fire in tabs *other* than the one that made the write).
//
// localStorage access is wrapped defensively throughout: Safari private
// browsing (older versions) and users with storage disabled can make
// `getItem`/`setItem` throw on every call, not just return null. On any such
// failure we fall back to the "native" default and keep serving in-memory
// updates for the current tab — persistence and cross-tab sync are best
// effort, but the toggle itself must never crash the app.
import { useSyncExternalStore } from "react";

export type DisplayCurrencyMode = "native" | "base";

const STORAGE_KEY = "babki.displayCurrency";
const DEFAULT_MODE: DisplayCurrencyMode = "native";

function isValidMode(value: unknown): value is DisplayCurrencyMode {
  return value === "native" || value === "base";
}

function readStoredMode(): DisplayCurrencyMode {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    return isValidMode(raw) ? raw : DEFAULT_MODE;
  } catch {
    return DEFAULT_MODE;
  }
}

function writeStoredMode(mode: DisplayCurrencyMode): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, mode);
  } catch {
    // Storage unavailable — the in-memory state still updates (see setMode),
    // so this tab keeps working; the choice just won't persist or cross tabs.
  }
}

let currentMode: DisplayCurrencyMode = readStoredMode();
const listeners = new Set<() => void>();

function notify(): void {
  for (const listener of listeners) listener();
}

function setMode(mode: DisplayCurrencyMode): void {
  if (!isValidMode(mode) || mode === currentMode) return;
  currentMode = mode;
  writeStoredMode(mode);
  notify();
}

function toggle(): void {
  setMode(currentMode === "native" ? "base" : "native");
}

// Cross-tab sync. `storage` fires only in tabs other than the one that made
// the write, so this is purely the "receive" side; the tab that calls
// setMode() above already notifies its own listeners directly. Guarded for
// non-browser environments (there are none in this SPA, but cheap to keep).
if (typeof window !== "undefined") {
  window.addEventListener("storage", (event) => {
    // event.key === null means the whole storage was cleared (e.g.
    // localStorage.clear()); anything else touching a different key is
    // irrelevant to this store.
    if (event.key !== null && event.key !== STORAGE_KEY) return;
    const next =
      event.key === null
        ? DEFAULT_MODE
        : isValidMode(event.newValue)
          ? event.newValue
          : DEFAULT_MODE;
    if (next !== currentMode) {
      currentMode = next;
      notify();
    }
  });
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function getSnapshot(): DisplayCurrencyMode {
  return currentMode;
}

export function useDisplayCurrency(): {
  mode: DisplayCurrencyMode;
  setMode: (mode: DisplayCurrencyMode) => void;
  toggle: () => void;
} {
  const mode = useSyncExternalStore(subscribe, getSnapshot, () => DEFAULT_MODE);
  return { mode, setMode, toggle };
}
