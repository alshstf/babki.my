import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { submitOnEnter } from "./submit-on-enter";

afterEach(cleanup);

describe("submitOnEnter", () => {
  it("leaves alone a field marked data-enter-ignore, and a checkbox", () => {
    const submit = vi.fn();
    render(
      <div onKeyDown={submitOnEnter(submit, true)}>
        <div data-enter-ignore>
          <input aria-label="search" />
        </div>
        <input aria-label="flag" type="checkbox" />
        <input aria-label="amount" />
      </div>,
    );
    fireEvent.keyDown(screen.getByLabelText("search"), { key: "Enter" });
    fireEvent.keyDown(screen.getByLabelText("flag"), { key: "Enter" });
    expect(submit).not.toHaveBeenCalled();
    fireEvent.keyDown(screen.getByLabelText("amount"), { key: "Enter" });
    expect(submit).toHaveBeenCalledTimes(1);
  });

  it("does not save while saving is not allowed", () => {
    const submit = vi.fn();
    render(
      <div onKeyDown={submitOnEnter(submit, false)}>
        <input aria-label="amount" />
      </div>,
    );
    fireEvent.keyDown(screen.getByLabelText("amount"), { key: "Enter" });
    expect(submit).not.toHaveBeenCalled();
  });
});
