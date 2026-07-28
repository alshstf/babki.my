import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { SettingsPage } from "./index";
import type { SessionInfo } from "@/api/session";

// jsdom doesn't implement scrollIntoView, but Radix's Select content calls
// it when positioning the open listbox — polyfill it as a no-op so the
// "open the select and click an item" flow doesn't throw.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

// SettingsPage reads the session straight from the query cache (useSession),
// so seeding ["session"] directly is the whole setup — no network mocking
// needed since none of these cases click Save (the only mutation trigger).
function wrap(ui: ReactElement, session: SessionInfo) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function makeSession(overrides: Partial<SessionInfo> = {}): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role: "owner",
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    ...overrides,
  };
}

describe("SettingsPage", () => {
  it("shows the base currency form for an owner with Save disabled until the currency changes", () => {
    wrap(<SettingsPage />, makeSession({ base_currency: "RUB" }));

    expect(screen.getByText("Базовая валюта")).toBeInTheDocument();
    expect(screen.getByRole("combobox")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Сохранить" })).toBeDisabled();
  });

  it("enables Save once a different currency is selected", () => {
    wrap(<SettingsPage />, makeSession({ base_currency: "RUB" }));

    fireEvent.click(screen.getByRole("combobox"));
    fireEvent.click(screen.getByText("USD"));

    expect(screen.getByRole("button", { name: "Сохранить" })).toBeEnabled();
  });

  it("shows an owner-only message and no form for a non-owner", () => {
    wrap(<SettingsPage />, makeSession({ role: "editor" }));

    expect(
      screen.getByText("Настройки доступны только владельцу пространства"),
    ).toBeInTheDocument();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Сохранить" })).not.toBeInTheDocument();
  });
});
