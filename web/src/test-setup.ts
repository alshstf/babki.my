import "@testing-library/jest-dom/vitest";

// Node 22+ defines its own global `localStorage` directly on `globalThis`
// (inert without the `--localstorage-file` CLI flag: every access warns once
// and getItem/setItem are no-ops). Vitest's jsdom environment aliases
// `window` to that same global object and only installs properties that
// aren't already present, so Node's inert accessor shadows jsdom's own
// working Storage implementation — any test exercising real localStorage
// persistence would silently see no-ops instead. Replace it with a minimal
// in-memory Storage so localStorage behaves like it does in a real browser.
function createMemoryStorage(): Storage {
  const data = new Map<string, string>();
  return {
    get length() {
      return data.size;
    },
    clear: () => data.clear(),
    getItem: (key: string) => (data.has(key) ? (data.get(key) ?? null) : null),
    key: (index: number) => Array.from(data.keys())[index] ?? null,
    removeItem: (key: string) => {
      data.delete(key);
    },
    setItem: (key: string, value: string) => {
      data.set(key, String(value));
    },
  };
}

if (typeof window !== "undefined") {
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: createMemoryStorage(),
  });
}
