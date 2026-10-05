import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { AccountTotal } from "./account-total";
import type { AccountTotal as AccountTotalPayload } from "@/api/positions";

const norm = (s: string) => s.replace(/[  ]/g, " ");

// The account's headline figure as published; the component adds nothing.
function makeTotal(
  overrides: Partial<AccountTotalPayload> = {},
): AccountTotalPayload {
  return {
    by_currency: [{ currency: "USD", amount_minor: 12_500 }],
    base_currency: "RUB",
    in_base: 1_000_000,
    in_base_gap: null,
    cash_fx_in_base: null,
    no_rate_currencies: [],
    undated_positions: 0,
    zero_valued_positions: 0,
    zero_valued_cost_by_currency: [],
    unknown_cost_positions: 0,
    ...overrides,
  };
}

describe("AccountTotal", () => {
  it("shows the base-currency figure as one number in base mode", () => {
    render(<AccountTotal total={makeTotal()} mode="base" />);

    const amounts = screen.getAllByTestId("account-total-amount");
    expect(amounts).toHaveLength(1);
    expect(norm(amounts[0].textContent ?? "")).toBe("10 000,00 ₽");
    expect(screen.getByTestId("account-total-label").textContent).toBe(
      "Всего заработано на счёте",
    );
  });

  it("shows one number per currency in the positions' own currencies", () => {
    // The owner's decision: rubles, dollars and yuan are three answers, never
    // summed into a number in no currency.
    render(
      <AccountTotal
        total={makeTotal({
          by_currency: [
            { currency: "CNY", amount_minor: 5_000 },
            { currency: "RUB", amount_minor: 250_000 },
            { currency: "USD", amount_minor: 12_500 },
          ],
        })}
        mode="native"
      />,
    );

    const shown = screen
      .getAllByTestId("account-total-amount")
      .map((el) => norm(el.textContent ?? ""));
    expect(shown).toEqual(["50,00 CN¥", "2 500,00 ₽", "125,00 $"]);
  });

  it("says which currencies have no total at all, instead of dropping them", () => {
    // A bucket short a term is null from the server and a sentence here, so
    // the list does not read as complete.
    render(
      <AccountTotal
        total={makeTotal({
          by_currency: [
            { currency: "RUB", amount_minor: 250_000 },
            { currency: "USD", amount_minor: null },
          ],
        })}
        mode="native"
      />,
    );

    expect(screen.getAllByTestId("account-total-amount")).toHaveLength(1);
    expect(
      screen.getByTestId("account-total-unknowable").textContent,
    ).toContain("USD");
  });

  it("says nothing was struck, and shows no number, when the base figure has a gap", () => {
    render(
      <AccountTotal
        total={makeTotal({ in_base: null, in_base_gap: "no_rate" })}
        mode="base"
      />,
    );

    expect(
      screen.queryByTestId("account-total-amount"),
    ).not.toBeInTheDocument();
    expect(screen.getByTestId("account-total-gap")).toBeInTheDocument();
  });

  it("names the money it could not value, rather than a bare «нет курса»", () => {
    // A source that quotes no rate for a currency (no CBR rate for XAU, the
    // broker's code for gold) has nothing to wait for; the sentence names
    // the currency.
    render(
      <AccountTotal
        total={makeTotal({
          in_base: null,
          in_base_gap: "no_rate",
          no_rate_currencies: ["XAU"],
        })}
        mode="base"
      />,
    );

    expect(screen.getByTestId("account-total-gap").textContent).toContain(
      "XAU",
    );
  });

  it("names the papers written off, and how much basis went in that way", () => {
    // The owner's chosen assumption, quantified: the total is lower by
    // exactly this much.
    render(
      <AccountTotal
        total={makeTotal({
          zero_valued_positions: 2,
          zero_valued_cost_by_currency: [
            { currency: "RUB", amount_minor: 5_000_000 },
          ],
        })}
        mode="base"
      />,
    );

    const mark = norm(
      screen.getByTestId("account-total-zero-valued").textContent ?? "",
    );
    expect(mark).toContain("2");
    expect(mark).toContain("50 000,00 ₽");
    // And the figure itself is still shown: this is a caveat, not a gap.
    expect(screen.getByTestId("account-total-amount")).toBeInTheDocument();
  });

  it("names the papers left out for want of a purchase date", () => {
    // A missing date never resolves, so the figure is published without
    // those papers and the count says so.
    render(
      <AccountTotal total={makeTotal({ undated_positions: 2 })} mode="base" />,
    );

    expect(screen.getByTestId("account-total-undated").textContent).toContain(
      "2",
    );
    // The figure itself is still shown: this is a caveat, not a gap.
    expect(screen.getByTestId("account-total-amount")).toBeInTheDocument();
  });

  it("names the papers whose price nobody recorded", () => {
    // The opposite direction: these push the total up; the two marks are
    // never confused.
    render(
      <AccountTotal
        total={makeTotal({ unknown_cost_positions: 3 })}
        mode="base"
      />,
    );

    expect(
      screen.getByTestId("account-total-unknown-cost").textContent,
    ).toContain("3");
    expect(
      screen.queryByTestId("account-total-zero-valued"),
    ).not.toBeInTheDocument();
  });

  it("says nothing at all for an account with nothing to say", () => {
    render(
      <AccountTotal
        total={makeTotal({ by_currency: [], in_base: null })}
        mode="native"
      />,
    );

    expect(screen.queryByTestId("account-total")).not.toBeInTheDocument();
  });
});

// Р-5: the currency's share is named apart, and only in the base
// currency.
describe("AccountTotal: the currency result on the money", () => {
  it("names the share in base mode", () => {
    render(<AccountTotal total={makeTotal({ cash_fx_in_base: 2_000_000 })} mode="base" />);
    expect(norm(screen.getByTestId("account-total-cash-fx").textContent ?? "")).toBe(
      "из них курсовая разница по деньгам: +20 000,00 ₽",
    );
  });

  it("says nothing when there is none, or in the positions' own currencies", () => {
    const { unmount } = render(<AccountTotal total={makeTotal({ cash_fx_in_base: 0 })} mode="base" />);
    expect(screen.queryByTestId("account-total-cash-fx")).toBeNull();
    unmount();
    render(<AccountTotal total={makeTotal({ cash_fx_in_base: 2_000_000 })} mode="native" />);
    expect(screen.queryByTestId("account-total-cash-fx")).toBeNull();
  });
});
