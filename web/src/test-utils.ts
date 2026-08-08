// visibleText is an element's text as a SIGHTED reader sees it: its
// textContent with every visually-hidden node taken out.
//
// The two used to be the same string and stopped being one with #31. A cell
// that explains a missing or unconverted figure now carries the explanation
// twice — once in a `title`, which is where a pointer finds it, and once as a
// `.sr-only` text node, which is the only route that reliably reaches a screen
// reader on a span nothing can focus. That second copy is in `textContent` like
// any other text. Assertions that a cell "shows exactly this number" mean what
// they always meant, and compare against this instead, so they keep failing if
// an explanation ever becomes visible.
//
// It reads the class rather than computed styles deliberately: no stylesheet is
// loaded under jsdom, so `getComputedStyle` would report every element visible
// here and this helper would quietly become a no-op that agreed with whatever
// it was handed — a test passing for the wrong reason, which is a thing this
// project has been bitten by three times.
export function visibleText(el: Element): string {
  const clone = el.cloneNode(true) as Element;
  for (const hidden of clone.querySelectorAll(".sr-only")) hidden.remove();
  return clone.textContent ?? "";
}

// announcedText is the other half of the same pair: an element's text as a
// reader who is NOT looking at the screen meets it — its textContent with
// everything marked `aria-hidden` taken out.
//
// The two together are what a cell explaining a missing figure has to satisfy,
// and neither alone says it. A cell that drew «—» and hid its reason in a
// `title` passed any check on textContent while announcing nothing but a dash;
// a cell that spelled the reason out in plain sight would pass a check on
// announced text while shouting a paragraph at everyone. Asserting both pins
// the arrangement: the eye gets the dash, the ear gets the sentence.
//
// Whitespace is squeezed because the markup's own indentation is not something
// anyone hears, and because the visible and hidden halves sit on separate
// lines: comparing against a sentence would otherwise be comparing against a
// sentence plus however the file happens to be wrapped.
export function announcedText(el: Element): string {
  const clone = el.cloneNode(true) as Element;
  for (const hidden of clone.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
  return (clone.textContent ?? "").replace(/\s+/g, " ").trim();
}
