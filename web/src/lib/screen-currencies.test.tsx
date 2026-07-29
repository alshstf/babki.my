import { afterEach, describe, expect, it } from "vitest";
import { memo, useState } from "react";
import { act, render, screen } from "@testing-library/react";
import {
  ScreenCurrencyCountProvider,
  useEffectiveDisplayCurrencyMode,
  useHasMultipleScreenCurrencies,
  useReportScreenCurrencies,
} from "./screen-currencies";
import { useDisplayCurrency } from "./display-currency";

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

  // The provider's context value must be referentially stable while `count`
  // is unchanged. It isn't a cosmetic detail: `ctx` is a dependency of
  // useReportScreenCurrencies' effect, so a fresh object on every render
  // would tear down and re-run that effect (setting the count to 0 and back)
  // on every unrelated re-render of AppLayout — and would re-render every
  // consumer of the context along with it. This test pins that with a
  // memoized consumer, which re-renders only if the context value's identity
  // actually changed.
  it("does not re-render consumers on an unrelated parent re-render", async () => {
    let consumerRenders = 0;
    const CountingConsumer = memo(function CountingConsumer() {
      consumerRenders++;
      useHasMultipleScreenCurrencies();
      return null;
    });
    const MemoReporter = memo(Reporter);
    const currencies = ["RUB", "USD"];

    function Parent() {
      // Stands in for any unrelated AppLayout state (e.g. the session query
      // settling) that re-renders the provider without touching the count.
      const [unrelated, setUnrelated] = useState(0);
      return (
        <ScreenCurrencyCountProvider>
          <button onClick={() => setUnrelated(unrelated + 1)}>bump</button>
          <CountingConsumer />
          <MemoReporter currencies={currencies} />
        </ScreenCurrencyCountProvider>
      );
    }

    render(<Parent />);
    const rendersBeforeBump = consumerRenders;

    await act(async () => {
      screen.getByRole("button").click();
    });

    expect(consumerRenders).toBe(rendersBeforeBump);
  });
});

// Stand-in for a screen's money cells: shows the mode actually applied to
// them, plus a button that writes the user's stored choice the way the
// header toggle does.
function ModeProbe() {
  const effective = useEffectiveDisplayCurrencyMode();
  const { mode, setMode } = useDisplayCurrency();
  return (
    <div>
      <div data-testid="effective">{effective}</div>
      <div data-testid="stored">{mode}</div>
      <button onClick={() => setMode("base")}>base</button>
    </div>
  );
}

describe("useEffectiveDisplayCurrencyMode", () => {
  afterEach(() => {
    // The display-currency store is module-level and shared across tests in
    // this file, so put it back the way a fresh page load would find it.
    window.localStorage.clear();
  });

  it("applies the stored base mode while the screen has more than one currency", async () => {
    render(
      <ScreenCurrencyCountProvider>
        <ModeProbe />
        <Reporter currencies={["RUB", "USD"]} />
      </ScreenCurrencyCountProvider>,
    );

    await act(async () => screen.getByRole("button").click());

    expect(screen.getByTestId("effective")).toHaveTextContent("base");
  });

  it("falls back to native when the screen has fewer than two currencies, keeping the stored choice intact", async () => {
    const { rerender } = render(
      <ScreenCurrencyCountProvider>
        <ModeProbe />
        <Reporter currencies={["RUB", "USD"]} />
      </ScreenCurrencyCountProvider>,
    );

    await act(async () => screen.getByRole("button").click());
    expect(screen.getByTestId("effective")).toHaveTextContent("base");

    // The screen now shows a single currency, so the header hides the
    // toggle. The mode must stop being applied along with it — otherwise the
    // user is stuck in a mode they can no longer switch off.
    rerender(
      <ScreenCurrencyCountProvider>
        <ModeProbe />
        <Reporter currencies={["RUB"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("effective")).toHaveTextContent("native");
    // ...but the choice itself is untouched: it is the user's, and it must
    // come back the moment the toggle does.
    expect(screen.getByTestId("stored")).toHaveTextContent("base");

    rerender(
      <ScreenCurrencyCountProvider>
        <ModeProbe />
        <Reporter currencies={["RUB", "EUR"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("effective")).toHaveTextContent("base");
  });

  it("is native outside a provider, where no screen can report anything", () => {
    render(<ModeProbe />);
    expect(screen.getByTestId("effective")).toHaveTextContent("native");
  });
});
