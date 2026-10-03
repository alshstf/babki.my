import { useEffect, useState } from "react";

// What a touch screen cannot show: the explanations this application keeps in
// `title` attributes appear on hover, and a finger does not hover. On a device
// with no hovering pointer, a tap on an element that carries one shows it in a
// bubble under the element; the next tap anywhere, or a scroll, hides it.
//
// Taps on links and controls are left alone — they do something of their own,
// and their names are already on them.
export function TouchTitles() {
  const [bubble, setBubble] = useState<{ text: string; x: number; y: number } | null>(null);

  useEffect(() => {
    const noHover = window.matchMedia?.("(hover: none)").matches ?? false;
    if (!noHover) return;
    const onClick = (e: MouseEvent) => {
      const target = e.target as HTMLElement | null;
      if (!target || target.closest("a, button, input, select, textarea, [role='button'], [role='menuitem']")) {
        setBubble(null);
        return;
      }
      const holder = target.closest<HTMLElement>("[title]");
      const text = holder?.getAttribute("title")?.trim();
      if (!holder || !text) {
        setBubble(null);
        return;
      }
      const rect = holder.getBoundingClientRect();
      setBubble((current) =>
        current?.text === text ? null : { text, x: rect.left + rect.width / 2, y: rect.bottom + 6 },
      );
    };
    const hide = () => setBubble(null);
    document.addEventListener("click", onClick);
    window.addEventListener("scroll", hide, { passive: true });
    return () => {
      document.removeEventListener("click", onClick);
      window.removeEventListener("scroll", hide);
    };
  }, []);

  if (!bubble) return null;
  const width = Math.min(320, window.innerWidth - 24);
  const left = Math.max(12, Math.min(bubble.x - width / 2, window.innerWidth - width - 12));
  return (
    <div
      role="tooltip"
      data-testid="touch-title"
      className="fixed z-50 whitespace-pre-line rounded-md border bg-popover px-3 py-2 text-xs text-popover-foreground shadow-md"
      style={{ top: bubble.y, left, width }}
    >
      {bubble.text}
    </div>
  );
}
