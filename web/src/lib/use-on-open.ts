import { useEffect } from "react";

// useOnOpen runs reset each time a dialog opens, so every opening starts from
// the values reset sets rather than from what the last one left behind.
export function useOnOpen(open: boolean, reset: () => void): void {
  useEffect(() => {
    if (open) reset();
    // On opening only: reset is a new closure on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);
}
