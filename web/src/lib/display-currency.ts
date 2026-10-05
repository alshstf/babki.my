// The display-currency mode: amounts in their own ("native") currency or in
// the space's base currency (the server's in_base figures). A per-browser
// preference in localStorage. A small module store behind useSyncExternalStore
// re-renders every user together, on changes from this tab or another (the
// `storage` event). Storage access is defensive: where it throws (old Safari
// private mode, disabled storage) the mode falls back to "native" in memory.
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

// Cross-tab sync: `storage` fires only in other tabs; this tab notifies
// its listeners directly in setMode().
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
} {
  const mode = useSyncExternalStore(subscribe, getSnapshot, () => DEFAULT_MODE);
  return { mode, setMode };
}
