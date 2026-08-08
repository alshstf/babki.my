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

  // One screen can have several independent sections that each know a piece
  // of the currency picture — the account detail screen has the positions
  // table (via the screen component) and the operations journal, which owns
  // its own paginated query and therefore its own currency set. The provider
  // must merge those sets, not let the last reporter to run win.
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
    // Records every value the header rendered with, not just the final one:
    // an implementation that drops everything on unmount and then lets the
    // surviving reporter re-report would settle on the right answer while
    // visibly blinking the toggle out and back in on the way there.
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

    // The second section goes away (e.g. an emptied journal renders a
    // placeholder instead of the table). The first one is still on screen and
    // still multi-currency, so the toggle must stay — steadily.
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

  // The provider's contexts must be referentially stable while nothing a
  // reporter said has changed. It isn't a cosmetic detail: the actions object
  // is a dependency of every reporter's effect, so a fresh one on every render
  // would tear that effect down and re-run it on every unrelated re-render of
  // AppLayout — and would re-render every consumer of the context along with
  // it. This test pins that with a memoized consumer, which re-renders only if
  // the value's identity actually changed.
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

// Stand-in for a screen: reports the currencies it draws and shows the mode
// actually applied to its money cells, plus a button that writes the user's
// stored choice the way the header toggle does.
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

// Writes the user's stored choice the way the header toggle does, from
// outside any component.
//
// It goes THROUGH the store rather than writing localStorage directly, and it
// has to: the store reads localStorage once, when the module is first
// imported, and a same-tab write fires no `storage` event for it to hear (that
// event is delivered to other tabs only, by design — see display-currency.ts).
// A test that seeded the key by hand would therefore be measuring whatever
// mode the previous test in this file happened to leave behind, and would pass
// or fail on test ORDER.
function storeMode(mode: DisplayCurrencyMode) {
  const { result, unmount } = renderHook(() => useDisplayCurrency());
  act(() => result.current.setMode(mode));
  unmount();
}

describe("useScreenCurrencies", () => {
  afterEach(() => {
    // The display-currency store is module-level and shared across tests in
    // this file, so put it back the way a fresh page load would find it —
    // both halves of it, since the in-memory value outlives localStorage.
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

    // The screen now shows a single currency, so the header hides the
    // toggle. The mode must stop being applied along with it — otherwise the
    // user is stuck in a mode they can no longer switch off.
    rerender(
      <ScreenCurrencyCountProvider>
        <ModeProbe currencies={["RUB"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("effective")).toHaveTextContent("native");
    // ...but the choice itself is untouched: it is the user's, and it must
    // come back the moment the toggle does.
    expect(screen.getByTestId("stored")).toHaveTextContent("base");

    rerender(
      <ScreenCurrencyCountProvider>
        <ModeProbe currencies={["RUB", "EUR"]} />
      </ScreenCurrencyCountProvider>,
    );
    expect(screen.getByTestId("effective")).toHaveTextContent("base");
  });

  // A screen's own set is not the whole of what it must convert: another
  // section of the same screen (the operations journal) reports separately,
  // and the mode the screen hands down to it has to account for that too.
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
    // Not a formality: outside a provider the screen still knows its own two
    // currencies, so an implementation that counted only those would apply
    // the stored mode here — on a screen whose header cannot render the
    // control to leave it by.
    storeMode("base");
    render(<ModeProbe currencies={["RUB", "USD"]} />);
    expect(screen.getByTestId("effective")).toHaveTextContent("native");
  });

  // #41. The currency set travels to the provider through an effect, which
  // runs after the render commits, so a screen that read its mode back out of
  // the provider could not have it on the render that knew the currencies:
  // the first frame was drawn in each row's own currency and the next one
  // converted, and the sums visibly changed under a reader who had asked for
  // the base currency. Every mode this screen renders with is recorded, not
  // just the one it settles on, because settling correctly is exactly what
  // the defect already did.
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
