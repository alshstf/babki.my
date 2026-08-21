import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { AccountTotal } from "./account-total";
import type { AccountTotal as AccountTotalPayload } from "@/api/positions";

const norm = (s: string) => s.replace(/[  ]/g, " ");

// The account's headline figure exactly as the server publishes it. This
// component adds nothing to it: every test here is about which of the server's
// numbers reaches the screen, and what is said beside it.
function makeTotal(
  overrides: Partial<AccountTotalPayload> = {},
): AccountTotalPayload {
  return {
    by_currency: [{ currency: "USD", amount_minor: 12_500 }],
    base_currency: "RUB",
    in_base: 1_000_000,
    in_base_gap: null,
    no_rate_currencies: [],
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
    // The owner's decision, and the only honest shape for this mode: an account
    // holding rubles, dollars and yuan has three answers here and no single one
    // — adding them would produce an integer denominated in nothing.
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
    // A bucket short a term is published as null by the server, and shown as a
    // sentence here: an account that quietly listed one fewer currency than it
    // holds would read as a complete answer.
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
        total={makeTotal({ in_base: null, in_base_gap: "undated" })}
        mode="base"
      />,
    );

    expect(
      screen.queryByTestId("account-total-amount"),
    ).not.toBeInTheDocument();
    expect(screen.getByTestId("account-total-gap")).toBeInTheDocument();
  });

  it("names the money it could not value, rather than a bare «нет курса»", () => {
    // «Нет курса» alone sends a reader to wait for a backfill. A source that
    // does not quote a currency at all has nothing to fetch — the Bank of
    // Russia publishes none for XAU, the code the broker uses for gold — and on
    // the owner's account that one holding took the total off three screens
    // with nothing saying which money was responsible.
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
    // The assumption the owner chose, quantified: the total is lower than the
    // truth by exactly this much, and a reader is told so rather than left to
    // wonder why a frozen fund seems to have cost them everything.
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

  it("names the papers whose price nobody recorded", () => {
    // The opposite direction: these push the total UP, because their whole
    // market value counts as profit. Both marks can be true at once and they
    // must never be confused for one another.
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
