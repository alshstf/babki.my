import { afterEach } from "vitest";

// pretendNarrow makes useNarrow see a phone for the rest of the test: jsdom
// has no matchMedia, so the full table is what every other test renders.
export function pretendNarrow() {
  window.matchMedia = ((query: string) => ({
    matches: query.includes("max-width"),
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia;
}

afterEach(() => {
  // @ts-expect-error jsdom has none; the tests that set one take it back.
  delete window.matchMedia;
});
