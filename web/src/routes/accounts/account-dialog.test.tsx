import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { AccountDialog } from "./account-dialog";
import type { SessionInfo } from "@/api/session";
import type { AccountWithBalance } from "@/api/accounts";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call: a body can be read only once.
function serve(status: number, body: unknown) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );
}

function makeSession(): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role: "owner",
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    full_valuation: "nav_and_foreign",
    tax_residency: "RU",
    cost_basis_rules: {
      country: "RU",
      method: "fifo",
      perimeter: "account",
      supported: true,
      notices: [],
    },
  };
}

const account: AccountWithBalance = {
  id: "acc-1",
  name: "Брокерский",
  type: "brokerage",
  currency: "RUB",
  institution: "Broker Co",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  valued_by_balance: false,
  counted_by: "balance",
};

function open(existing?: AccountWithBalance) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], makeSession());
  return render(
    <QueryClientProvider client={qc}>
      <AccountDialog open onOpenChange={() => {}} account={existing} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  fetchMock.mockReset();
});

// #95: the server's English log prose was printed in the red panel.
describe("AccountDialog — a refusal from the server", () => {
  it("says a new account was not created, in Russian", async () => {
    serve(400, {
      error: "name is required, type must be valid, currency must be ISO-4217 uppercase",
    });
    open();
    fireEvent.change(screen.getByLabelText("Название"), { target: { value: "Новый" } });
    fireEvent.click(screen.getByRole("button", { name: "Создать" }));

    expect(await screen.findByText("Не удалось создать счет")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("ISO-4217 uppercase");
  });

  it("says the changes were not saved, in Russian", async () => {
    // «Не удалось создать» would be false over an edit.
    serve(404, { error: "not found" });
    open(account);
    fireEvent.change(screen.getByLabelText("Название"), { target: { value: "Другое имя" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));

    expect(await screen.findByText("Не удалось сохранить счет")).toBeInTheDocument();
    expect(screen.queryByText("Не удалось создать счет")).toBeNull();
  });
});

// Every field has an accessible name: a select by its label, not its
// value.
describe("AccountDialog — every field is named", () => {
  it("names each field by its label", async () => {
    open();
    // jsdom lacks scrollIntoView, which Radix Select calls on open.
    Element.prototype.scrollIntoView = () => {};
    fireEvent.click(screen.getByRole("combobox", { name: "Валюта" }));
    fireEvent.click(await screen.findByRole("option", { name: "Другая…" }));
    const fields = [...screen.getAllByRole("combobox"), ...screen.getAllByRole("textbox")];
    expect(fields.length).toBeGreaterThan(3);
    for (const field of fields) {
      expect(field).toHaveAccessibleName();
    }
    expect(screen.getByRole("combobox", { name: "Тип" })).toBeInTheDocument();
  });
});
