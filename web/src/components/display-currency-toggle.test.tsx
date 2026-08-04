import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { DisplayCurrencyToggle } from "./display-currency-toggle";

// The mode lives in a module-level store read through useSyncExternalStore
// (see lib/display-currency.ts), so there is no provider to wrap this in.
function wrap(visible = true) {
  return render(<DisplayCurrencyToggle visible={visible} />);
}

describe("DisplayCurrencyToggle", () => {
  it("names the two modes by what they show", () => {
    wrap();
    expect(screen.getByRole("button", { name: "в исходной валюте" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "в базовой" })).toBeInTheDocument();
  });

  it("does not call the unconverted mode the account's currency", () => {
    // #108. The label used to read «в валюте счёта», and on two of the three
    // screens this toggle governs that is simply not what the mode shows: a
    // position row is in the POSITION's currency and a journal row in the
    // OPERATION's, neither of which has to be the account's. The demo stand
    // makes it concrete — the ruble Т-Банк account holds dollar rows, and the
    // toggle offered to show them «в валюте счёта» while showing dollars.
    //
    // «в исходной валюте» is the currency each figure is denominated in
    // whatever kind of row it sits on, and it is the phrase the unconverted
    // captions on the positions screen already use for the same thing.
    wrap();
    expect(screen.queryByRole("button", { name: "в валюте счёта" })).not.toBeInTheDocument();
    const group = screen.getByRole("group");
    expect(group.getAttribute("aria-label") ?? "").not.toContain("каждого счета");
  });

  it("renders nothing when the screen has only one currency to show", () => {
    wrap(false);
    expect(screen.queryByRole("group")).not.toBeInTheDocument();
  });
});
