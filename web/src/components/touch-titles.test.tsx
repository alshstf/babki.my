import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { TouchTitles } from "./touch-titles";

function hoverless(noHover: boolean) {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: query === "(hover: none)" ? noHover : false,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("TouchTitles", () => {
  it("shows a tapped hint on a screen with no hover, and hides it on the next tap", () => {
    hoverless(true);
    render(
      <>
        <TouchTitles />
        <span title="цена от 25.02.2022">устарела</span>
        <p>elsewhere</p>
      </>,
    );
    act(() => {
      fireEvent.click(screen.getByText("устарела"));
    });
    expect(screen.getByTestId("touch-title").textContent).toBe("цена от 25.02.2022");
    act(() => {
      fireEvent.click(screen.getByText("elsewhere"));
    });
    expect(screen.queryByTestId("touch-title")).toBeNull();
  });

  it("leaves links and buttons to do their own thing", () => {
    hoverless(true);
    render(
      <>
        <TouchTitles />
        <button title="Выйти">x</button>
      </>,
    );
    act(() => {
      fireEvent.click(screen.getByRole("button"));
    });
    expect(screen.queryByTestId("touch-title")).toBeNull();
  });

  it("does nothing where a pointer can hover", () => {
    hoverless(false);
    render(
      <>
        <TouchTitles />
        <span title="подсказка">x</span>
      </>,
    );
    act(() => {
      fireEvent.click(screen.getByText("x"));
    });
    expect(screen.queryByTestId("touch-title")).toBeNull();
  });
});
