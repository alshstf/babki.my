import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import {
  ScreenCurrencyCountProvider,
  useHasMultipleScreenCurrencies,
  useReportScreenCurrencies,
} from "./screen-currencies";

// Stand-in for a screen: reports whatever currency set it's given for as
// long as it stays mounted.
function Reporter({ currencies }: { currencies: string[] }) {
  useReportScreenCurrencies(currencies);
  return null;
}

// Stand-in for the header: renders a visible marker only when the provider
// says more than one currency is in play.
function ToggleProbe() {
  const visible = useHasMultipleScreenCurrencies();
  return <div data-testid="toggle">{visible ? "visible" : "hidden"}</div>;
}

describe("screen-currencies", () => {
  it("reports hidden with no reporter mounted (default state)", () => {
    render(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("hidden");
  });

  it("stays hidden when the reporting screen has exactly one currency", () => {
    render(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <Reporter currencies={["RUB"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("hidden");
  });

  it("de-duplicates repeated currencies before counting", () => {
    render(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <Reporter currencies={["RUB", "RUB", "RUB"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("hidden");
  });

  it("becomes visible when the reporting screen has more than one currency", () => {
    render(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <Reporter currencies={["RUB", "USD"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("visible");
  });

  it("resets to hidden when the reporting screen unmounts (navigating away)", () => {
    const { rerender } = render(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <Reporter currencies={["RUB", "USD"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("visible");

    // Simulate navigating to a screen that never calls useReportScreenCurrencies
    // at all (e.g. /family) — the count must not linger from the old screen.
    rerender(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("hidden");
  });

  it("updates live when the reporting screen's own currency set changes", () => {
    const { rerender } = render(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <Reporter currencies={["RUB"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("hidden");

    rerender(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <Reporter currencies={["RUB", "EUR"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("visible");
  });

  it("is hidden outside of a provider (defensive default, no crash)", () => {
    render(<ToggleProbe />);
    expect(screen.getByTestId("toggle")).toHaveTextContent("hidden");
  });
});
