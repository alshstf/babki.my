import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { DisplayCurrencyToggle } from "./display-currency-toggle";

// The mode is a module-level store, so there is no provider.
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
    // #108: position and journal rows are in their own currency, not the
    // account's (the demo's rouble Т-Банк account holds dollar rows), so the
    // label is «в исходной валюте», as the positions captions say.
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
