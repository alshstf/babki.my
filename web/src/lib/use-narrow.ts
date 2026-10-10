import { useSyncExternalStore } from "react";

// Below Tailwind's sm: a phone held upright.
const NARROW = "(max-width: 639px)";

function subscribe(onChange: () => void): () => void {
  if (typeof window.matchMedia !== "function") return () => {};
  const query = window.matchMedia(NARROW);
  query.addEventListener("change", onChange);
  return () => query.removeEventListener("change", onChange);
}

// useNarrow says whether the screen is a phone's, for tables that fold their
// side columns under the main one there. Rendering one layout or the other,
// rather than hiding columns with CSS, keeps each figure on the page once.
// False where the browser cannot say (tests), so the full table is the default.
export function useNarrow(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => typeof window.matchMedia === "function" && window.matchMedia(NARROW).matches,
    () => false,
  );
}
