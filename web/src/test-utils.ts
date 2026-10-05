// visibleText is an element's text as a sighted reader sees it: textContent
// without visually hidden nodes. A cell explaining a figure carries the
// explanation in a `title` and in a `.sr-only` node (#31); assertions that
// a cell "shows exactly this number" compare against this.
//
// It reads the class, not computed styles: jsdom loads no stylesheet, so
// getComputedStyle would call everything visible and make this a no-op.
export function visibleText(el: Element): string {
  const clone = el.cloneNode(true) as Element;
  for (const hidden of clone.querySelectorAll(".sr-only")) hidden.remove();
  return clone.textContent ?? "";
}

// announcedText is an element's text as a screen-reader user meets it:
// textContent without `aria-hidden` nodes, whitespace squeezed. Asserted
// together with visibleText, it pins the arrangement: the eye gets the
// dash, the ear gets the sentence.
export function announcedText(el: Element): string {
  const clone = el.cloneNode(true) as Element;
  for (const hidden of clone.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
  return (clone.textContent ?? "").replace(/\s+/g, " ").trim();
}
