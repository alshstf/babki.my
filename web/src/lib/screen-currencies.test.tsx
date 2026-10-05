import { afterEach, describe, expect, it } from "vitest";
import { memo, useState } from "react";
import { act, render, renderHook, screen } from "@testing-library/react";
import {
  ScreenCurrencyCountProvider,
  useHasMultipleScreenCurrencies,
  useReportScreenCurrencies,
  useScreenCurrencies,
} from "./screen-currencies";
import { useDisplayCurrency, type DisplayCurrencyMode } from "./display-currency";

// Stands in for a screen: reports its currency set while mounted.
function Reporter({ currencies }: { currencies: string[] }) {
  useReportScreenCurrencies(currencies);
  return null;
}

// Stands in for the header: a marker only for more than one currency.
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

    // A screen that never reports (e.g. /family): the count must not linger.
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

  // One screen can have several reporters (the account screen's positions
  // and its journal); the provider merges their sets.
  it("counts the union of two simultaneous reporters, not just the last one", () => {
    render(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <Reporter currencies={["RUB"]} />
        <Reporter currencies={["USD"]} />
      </ScreenCurrencyCountProvider>,
    );
    // Neither reporter alone has anything to convert; together they do.
    expect(screen.getByTestId("toggle")).toHaveTextContent("visible");
  });

  it("is order-independent: the same two reporters in the other order agree", () => {
    render(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <Reporter currencies={["USD"]} />
        <Reporter currencies={["RUB"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("visible");
  });

  it("keeps a still-mounted reporter's currencies when another reporter unmounts, without blinking", () => {
    // Every rendered value, so a toggle that blinks out and back in on the
    // way to the right answer fails.
    const rendered: boolean[] = [];
    function RecordingProbe() {
      const visible = useHasMultipleScreenCurrencies();
      rendered.push(visible);
      return <div data-testid="toggle">{visible ? "visible" : "hidden"}</div>;
    }

    const { rerender } = render(
      <ScreenCurrencyCountProvider>
        <RecordingProbe />
        <Reporter currencies={["RUB", "USD"]} />
        <Reporter currencies={["EUR"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("visible");

    // The second section goes away; the first is still multi-currency, so
    // the toggle stays, steadily.
    rendered.length = 0;
    rerender(
      <ScreenCurrencyCountProvider>
        <RecordingProbe />
        <Reporter currencies={["RUB", "USD"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("visible");
    expect(rendered).not.toContain(false);
  });

  it("drops only the unmounted reporter's contribution to the union", () => {
    const { rerender } = render(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <Reporter currencies={["RUB"]} />
        <Reporter currencies={["USD"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("visible");

    rerender(
      <ScreenCurrencyCountProvider>
        <ToggleProbe />
        <Reporter currencies={["RUB"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("toggle")).toHaveTextContent("hidden");
  });

  // The contexts stay referentially stable while no report changed: the
  // actions object is a dependency of every reporter's effect. A memoized
  // consumer re-renders only on a new identity.
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
      // Unrelated AppLayout state that re-renders the provider.
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

// Stands in for a screen: reports its currencies, shows the applied
// mode, and writes the stored choice as the header toggle does.
function ModeProbe({ currencies = [] }: { currencies?: string[] }) {
  const effective = useScreenCurrencies(currencies);
  const { mode, setMode } = useDisplayCurrency();
  return (
    <div>
      <div data-testid="effective">{effective}</div>
      <div data-testid="stored">{mode}</div>
      <button onClick={() => setMode("base")}>base</button>
    </div>
  );
}

// Through the store, not localStorage: the store reads localStorage once
// on import and hears no same-tab `storage` event, so a hand-seeded key
// would make tests depend on order.
function storeMode(mode: DisplayCurrencyMode) {
  const { result, unmount } = renderHook(() => useDisplayCurrency());
  act(() => result.current.setMode(mode));
  unmount();
}

describe("useScreenCurrencies", () => {
  afterEach(() => {
    // Reset the shared store both in memory and in localStorage.
    storeMode("native");
    window.localStorage.clear();
  });

  it("applies the stored base mode while the screen has more than one currency", async () => {
    render(
      <ScreenCurrencyCountProvider>
        <ModeProbe currencies={["RUB", "USD"]} />
      </ScreenCurrencyCountProvider>,
    );

    await act(async () => screen.getByRole("button").click());

    expect(screen.getByTestId("effective")).toHaveTextContent("base");
  });

  it("falls back to native when the screen has fewer than two currencies, keeping the stored choice intact", async () => {
    const { rerender } = render(
      <ScreenCurrencyCountProvider>
        <ModeProbe currencies={["RUB", "USD"]} />
      </ScreenCurrencyCountProvider>,
    );

    await act(async () => screen.getByRole("button").click());
    expect(screen.getByTestId("effective")).toHaveTextContent("base");

    // One currency hides the toggle, so the mode stops applying; otherwise
    // the user is stuck in it.
    rerender(
      <ScreenCurrencyCountProvider>
        <ModeProbe currencies={["RUB"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("effective")).toHaveTextContent("native");
    // ...but the stored choice is kept for when the toggle returns.
    expect(screen.getByTestId("stored")).toHaveTextContent("base");

    rerender(
      <ScreenCurrencyCountProvider>
        <ModeProbe currencies={["RUB", "EUR"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("effective")).toHaveTextContent("base");
  });

  // Another section of the screen reports separately, and the mode handed
  // down accounts for it.
  it("applies the stored mode when only another section's currencies make the screen multi-currency", async () => {
    render(
      <ScreenCurrencyCountProvider>
        <ModeProbe currencies={["RUB"]} />
        <Reporter currencies={["USD"]} />
      </ScreenCurrencyCountProvider>,
    );

    await act(async () => screen.getByRole("button").click());

    expect(screen.getByTestId("effective")).toHaveTextContent("base");
  });

  it("is native outside a provider, where the header has no toggle to switch back", () => {
    // Outside a provider the screen knows only its own currencies; applying
    // the mode here would leave no control to leave it by.
    storeMode("base");
    render(<ModeProbe currencies={["RUB", "USD"]} />);
    expect(screen.getByTestId("effective")).toHaveTextContent("native");
  });

  // #41: the set reaches the provider through an effect, so reading the
  // mode back from it drew the first frame native and the next converted.
  // Every rendered mode is recorded, since settling correctly is what the
  // defect already did.
  it("renders in the stored mode from its very first frame, never a native one first", () => {
    storeMode("base");
    const modes: DisplayCurrencyMode[] = [];
    function RecordingScreen() {
      const mode = useScreenCurrencies(["RUB", "USD"]);
      modes.push(mode);
      return null;
    }

    render(
      <ScreenCurrencyCountProvider>
        <RecordingScreen />
      </ScreenCurrencyCountProvider>,
    );

    expect(modes[0]).toBe("base");
    expect(modes).not.toContain("native");
  });
});
