import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { MoneyCell } from "./money-cell";
import { formatMinor } from "@/lib/money";

describe("MoneyCell", () => {
  it("renders the amount with no indicator when noRate is false", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 100_00, currency: "USD", noRate: false, converted: false, rateOn: null }}
        testId="amt"
      />,
    );

    const el = screen.getByTestId("amt");
    expect(el.textContent).toBe(formatMinor(100_00, "USD"));
    expect(screen.queryByTestId("amt-not-converted")).not.toBeInTheDocument();
    // Nothing was converted, so there is no rate date to disclose.
    expect(el).not.toHaveAttribute("title");
  });

  it("discloses the fx rate date in the cell's tooltip when the amount was converted", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 900_000, currency: "RUB", noRate: false, converted: true, rateOn: "2026-07-20" }}
        testId="amt"
      />,
    );

    const el = screen.getByTestId("amt");
    // The date stays out of the cell text — tooltip only (owner preference).
    expect(el.textContent).toBe(formatMinor(900_000, "RUB"));
    expect(el.textContent).not.toMatch(/20\.07\.2026/);
    expect(el).toHaveAttribute("title", "Пересчитано по текущему курсу (на 20.07.2026)");
  });

  it("omits the tooltip when the rate date is unparseable rather than showing a broken one", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 900_000, currency: "RUB", noRate: false, converted: true, rateOn: "garbage" }}
        testId="amt"
      />,
    );

    expect(screen.getByTestId("amt")).not.toHaveAttribute("title");
  });

  it("renders the native amount plus a tooltipped indicator when noRate is true", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 100_00, currency: "USD", noRate: true, converted: false, rateOn: null }}
        testId="amt"
      />,
    );

    const el = screen.getByTestId("amt");
    // The amount text itself is still the honest native figure (no dash, no zero).
    expect(el.textContent).toContain(formatMinor(100_00, "USD"));

    const indicator = screen.getByTestId("amt-not-converted");
    expect(indicator).toHaveAttribute("title", "Нет курса — показано в валюте счёта");
  });

  it("omits the indicator element entirely when testId is not provided, but still shows it visually", () => {
    render(<MoneyCell resolved={{ amountMinor: 5_00, currency: "EUR", noRate: true, converted: false, rateOn: null }} />);

    expect(screen.getByTitle("Нет курса — показано в валюте счёта")).toBeInTheDocument();
  });

  it("uses a caller-supplied not-converted wording when the native currency is not an account's", () => {
    // Position rows show amounts in the position's / quote's / bond face
    // value's currency, none of which is "the account's currency" — so the
    // default wording would name the wrong thing.
    render(
      <MoneyCell
        resolved={{ amountMinor: 100_00, currency: "EUR", noRate: true, converted: false, rateOn: null }}
        notConvertedTitle="Нет курса — показано в исходной валюте"
        testId="amt"
      />,
    );

    expect(screen.getByTestId("amt-not-converted")).toHaveAttribute(
      "title",
      "Нет курса — показано в исходной валюте",
    );
  });

  it("uses a caller-supplied converted-title wording when the rate is not today's", () => {
    // The operations journal converts at the rate in effect on the operation's
    // own date, so the default wording — which describes a current rate —
    // would misrepresent the number.
    render(
      <MoneyCell
        resolved={{ amountMinor: 655_000, currency: "RUB", noRate: false, converted: true, rateOn: "2019-03-12" }}
        convertedTitle={(date) => `Пересчитано по курсу на дату операции — ${date}`}
        testId="amt"
      />,
    );

    expect(screen.getByTestId("amt")).toHaveAttribute(
      "title",
      "Пересчитано по курсу на дату операции — 12.03.2019",
    );
  });

  it("still uses the caller-supplied converted-title wording when the converted figure has no rate date", () => {
    // A converted figure does not always have ONE date behind it: a position's
    // cost is struck at one rate per purchase day, and the object carrying it
    // publishes rate_on only when it also carries the market valuation that
    // date belongs to (see PositionInBase.rate_on in the API contract). The
    // wording here says nothing about a date, so a null one is no reason to
    // withhold it — and withholding it is exactly what keying the tooltip off
    // the date used to do, silently, on every position without a quote.
    render(
      <MoneyCell
        resolved={{ amountMinor: 9_000_000, currency: "RUB", noRate: false, converted: true, rateOn: null }}
        convertedTitle={() => "Пересчитано по курсам на даты покупок, а не по текущему"}
        testId="amt"
      />,
    );

    expect(screen.getByTestId("amt")).toHaveAttribute(
      "title",
      "Пересчитано по курсам на даты покупок, а не по текущему",
    );
  });

  it("omits the default converted wording when the converted figure has no rate date", () => {
    // The twin of the test above, for the fallback wording: "converted at the
    // current rate (on <date>)" has nowhere to put a date it does not have, and
    // a dangling sentence reads worse than silence.
    render(
      <MoneyCell
        resolved={{ amountMinor: 9_000_000, currency: "RUB", noRate: false, converted: true, rateOn: null }}
        testId="amt"
      />,
    );

    expect(screen.getByTestId("amt")).not.toHaveAttribute("title");
  });

  it("does not call the caller-supplied converted-title wording when nothing was converted", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 100_00, currency: "USD", noRate: true, converted: false, rateOn: null }}
        convertedTitle={(date) => `никогда — ${date}`}
        testId="amt"
      />,
    );

    expect(screen.getByTestId("amt")).not.toHaveAttribute("title");
  });

  it("applies the given className to the root element", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 100_00, currency: "USD", noRate: false, converted: false, rateOn: null }}
        className="text-2xl font-bold"
        testId="amt"
      />,
    );

    expect(screen.getByTestId("amt").className).toContain("text-2xl");
  });
});
