import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { MoneyCell } from "./money-cell";
import { formatMinor } from "@/lib/money";
import { announcedText, visibleText } from "@/test-utils";

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
    // Position rows show the position's, quote's or face's currency, not the
    // account's, so the default wording would name the wrong thing.
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
    // The journal converts at the operation date's rate; the default wording
    // describes a current rate.
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
    // A position's cost has no single date (rate_on is published only with a
    // market valuation). This wording names no date, so a null one does not
    // withhold it.
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
    // The fallback wording names a date, so without one it is withheld.
    render(
      <MoneyCell
        resolved={{ amountMinor: 9_000_000, currency: "RUB", noRate: false, converted: true, rateOn: null }}
        testId="amt"
      />,
    );

    expect(screen.getByTestId("amt")).not.toHaveAttribute("title");
  });

  it("hands the caller a null date, not an empty string, when the rate date does not parse", () => {
    // The caller decides what an unusable date means for its wording, so it
    // gets null rather than an empty string.
    const seen: (string | null)[] = [];
    render(
      <MoneyCell
        resolved={{ amountMinor: 655_000, currency: "RUB", noRate: false, converted: true, rateOn: "2019-13-99" }}
        convertedTitle={(date) => {
          seen.push(date);
          return date ? `на ${date}` : undefined;
        }}
        testId="amt"
      />,
    );

    expect(seen).toEqual([null]);
    expect(screen.getByTestId("amt")).not.toHaveAttribute("title");
  });

  it("shows a caveat about what the figure is alongside the one about its currency", () => {
    // Two statements about one number, each with its own indicator.
    render(
      <MoneyCell
        resolved={{ amountMinor: 190_000, currency: "USD", noRate: true, converted: false, rateOn: null }}
        notConvertedTitle="Нет курса на дату операции — показано в валюте операции"
        caveatTitle="Это стоимость бумаг — её выбрало правило очереди"
        testId="amt"
      />,
    );

    expect(screen.getByTestId("amt-not-converted")).toHaveAttribute(
      "title",
      "Нет курса на дату операции — показано в валюте операции",
    );
    expect(screen.getByTestId("amt-caveat")).toHaveAttribute(
      "title",
      "Это стоимость бумаг — её выбрало правило очереди",
    );
  });

  it("renders no caveat indicator when the caller has nothing to qualify", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 100_00, currency: "USD", noRate: false, converted: false, rateOn: null }}
        testId="amt"
      />,
    );

    expect(screen.queryByTestId("amt-caveat")).not.toBeInTheDocument();
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

  // #31: an icon's title on a focusless, roleless span reaches a pointer
  // only. The glyph is decorative and the sentence is spelled out for a
  // screen reader; `title` stays for the pointer. visibleText and
  // announcedText are asserted as a pair (see test-utils).
  it("spells the not-converted sentence out for a screen reader, not only in a title", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 100_00, currency: "USD", noRate: true, converted: false, rateOn: null }}
        testId="amt"
      />,
    );

    const indicator = screen.getByTestId("amt-not-converted");
    expect(visibleText(indicator)).toBe("");
    expect(announcedText(indicator)).toBe("Нет курса — показано в валюте счёта");
    expect(indicator).toHaveAttribute("title", "Нет курса — показано в валюте счёта");
    // The caller's wording is what gets said: this component never picks
    // the cause.
    expect(visibleText(screen.getByTestId("amt"))).toBe(formatMinor(100_00, "USD"));
  });

  it("spells the caller's own not-converted wording out, not the default one", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 100_00, currency: "USD", noRate: true, converted: false, rateOn: null }}
        notConvertedTitle="Нет курса на день покупки"
        testId="amt"
      />,
    );

    expect(announcedText(screen.getByTestId("amt-not-converted"))).toBe(
      "Нет курса на день покупки",
    );
  });

  it("spells the caveat out for a screen reader too", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 100_00, currency: "USD", noRate: false, converted: false, rateOn: null }}
        caveatTitle="Это стоимость бумаг, а не деньги этого дня"
        testId="amt"
      />,
    );

    const caveat = screen.getByTestId("amt-caveat");
    expect(visibleText(caveat)).toBe("");
    expect(announcedText(caveat)).toBe("Это стоимость бумаг, а не деньги этого дня");
    expect(caveat).toHaveAttribute("title", "Это стоимость бумаг, а не деньги этого дня");
  });
});
