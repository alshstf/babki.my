import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import "@/i18n";
import type { Operation } from "@/api/operations";
import { WithheldAbroadNote } from "./withheld-abroad";

type Withheld = NonNullable<Operation["withheld_abroad"]>;

const norm = (s: string | null | undefined) => (s ?? "").replace(/[  ]/g, " ");

const estimated: Withheld = {
  state: "estimated",
  currency: "USD",
  received_minor: 14,
  gross_minor: 20,
  tax_minor: 6,
  rate_percent: "30.0",
  per_share: "0.04",
  shares: "5",
  record_date: "2021-09-02",
  tax_in_base: { currency: "RUB", amount_minor: 438 },
  broker_tax: [],
};

afterEach(cleanup);

describe("WithheldAbroadNote", () => {
  it("shows the estimate as one, with its arithmetic in the tooltip", () => {
    render(<WithheldAbroadNote withheld={estimated} />);

    const line = screen.getByTestId("operation-withheld-estimate");
    expect(norm(line.textContent)).toBe("Налог у источника ≈ 0,06 $ (30,0 %), ≈ 4,38 ₽ по курсу дня выплаты");
    expect(norm(line.getAttribute("title"))).toBe(
      "Оценка по календарю дивидендов: объявлено 0,04 $ на акцию × 5 шт. на 02.09.2021 = 0,20 $ до удержания, пришло 0,14 $. Это не документ — сверьте со справкой брокера",
    );
  });

  it("says the sum is unknown and why, never a guess", () => {
    render(
      <WithheldAbroadNote
        withheld={{
          ...estimated,
          state: "unknown",
          unknown_reason: "another_currency",
          gross_minor: null,
          tax_minor: null,
          rate_percent: null,
          tax_in_base: null,
          currency: "RUB",
          broker_tax: [{ currency: "RUB", amount_minor: -1600 }],
        }}
      />,
    );

    const line = screen.getByTestId("operation-withheld-unknown");
    expect(line.textContent).toBe("Налог у источника: сумма неизвестна");
    expect(line.getAttribute("title")).toContain("в другой валюте");
    expect(screen.queryByTestId("operation-withheld-estimate")).toBeNull();
    expect(norm(screen.getByTestId("operation-withheld-broker").textContent)).toBe("Налог строкой брокера: -16,00 ₽");
  });

  it("leaves a payment taxed by the broker's own row to that row", () => {
    render(
      <WithheldAbroadNote
        withheld={{ ...estimated, state: "reported", gross_minor: null, tax_minor: null, rate_percent: null, tax_in_base: null, broker_tax: [{ currency: "USD", amount_minor: -2 }] }}
      />,
    );

    expect(screen.queryByTestId("operation-withheld-estimate")).toBeNull();
    expect(screen.queryByTestId("operation-withheld-unknown")).toBeNull();
    expect(norm(screen.getByTestId("operation-withheld-broker").textContent)).toBe("Налог строкой брокера: -0,02 $");
  });
});
