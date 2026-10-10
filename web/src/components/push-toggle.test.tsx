import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import "@/i18n";
import { PushToggle } from "./push-toggle";
import { keyBytes } from "@/api/push";

afterEach(cleanup);

describe("push", () => {
  it("turns the server's base64url key into its bytes", () => {
    // An uncompressed P-256 point starts with 4; "-" and "_" are url-safe.
    const bytes = keyBytes("BP-_");
    expect(Array.from(bytes)).toEqual([4, 255, 191]);
  });

  it("says how to get reminders where the browser has no push", async () => {
    render(<PushToggle />);
    expect((await screen.findByTestId("push-toggle")).textContent).toMatch(/в установленном приложении/);
    expect(screen.queryByRole("button")).toBeNull();
  });
});
