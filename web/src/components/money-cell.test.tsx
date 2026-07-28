import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { MoneyCell } from "./money-cell";
import { formatMinor } from "@/lib/money";

describe("MoneyCell", () => {
  it("renders the amount with no indicator when noRate is false", () => {
    render(<MoneyCell resolved={{ amountMinor: 100_00, currency: "USD", noRate: false }} testId="amt" />);

    const el = screen.getByTestId("amt");
    expect(el.textContent).toBe(formatMinor(100_00, "USD"));
    expect(screen.queryByTestId("amt-not-converted")).not.toBeInTheDocument();
  });

  it("renders the native amount plus a tooltipped indicator when noRate is true", () => {
    render(<MoneyCell resolved={{ amountMinor: 100_00, currency: "USD", noRate: true }} testId="amt" />);

    const el = screen.getByTestId("amt");
    // The amount text itself is still the honest native figure (no dash, no zero).
    expect(el.textContent).toContain(formatMinor(100_00, "USD"));

    const indicator = screen.getByTestId("amt-not-converted");
    expect(indicator).toHaveAttribute("title", "Нет курса — показано в валюте счёта");
  });

  it("omits the indicator element entirely when testId is not provided, but still shows it visually", () => {
    render(<MoneyCell resolved={{ amountMinor: 5_00, currency: "EUR", noRate: true }} />);

    expect(screen.getByTitle("Нет курса — показано в валюте счёта")).toBeInTheDocument();
  });

  it("applies the given className to the root element", () => {
    render(
      <MoneyCell
        resolved={{ amountMinor: 100_00, currency: "USD", noRate: false }}
        className="text-2xl font-bold"
        testId="amt"
      />,
    );

    expect(screen.getByTestId("amt").className).toContain("text-2xl");
  });
});
