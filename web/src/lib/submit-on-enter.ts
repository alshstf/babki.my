import type { KeyboardEvent } from "react";

// submitOnEnter is a dialog's keydown handler that saves on Enter pressed in
// one of its text fields — what a form does, without turning every button
// inside the dialog into a submit button. It does nothing while the dialog
// cannot be saved, inside an element marked data-enter-ignore (a search box
// whose Enter means something of its own), or when Enter only finishes typing
// a character composed from several keys.
export function submitOnEnter(submit: () => void, enabled: boolean) {
  return (e: KeyboardEvent<HTMLElement>) => {
    if (e.key !== "Enter" || e.shiftKey || e.nativeEvent.isComposing) return;
    const target = e.target as HTMLElement;
    if (target.tagName !== "INPUT") return;
    const type = (target as HTMLInputElement).type;
    if (type === "checkbox" || type === "radio" || type === "button" || type === "submit") return;
    if (target.closest("[data-enter-ignore]")) return;
    e.preventDefault();
    if (enabled) submit();
  };
}
