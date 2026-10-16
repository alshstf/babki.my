import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { RatePanel } from "./rate-panel";
import type { AccountWithBalance } from "@/api/accounts";
import type { AccountRates } from "@/api/account-rates";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

const deposit: AccountWithBalance = {
  id: "dep-1", name: "Вклад", type: "deposit", currency: "RUB", institution: "", status: "active",
  created_at: "2026-01-01T00:00:00Z", valued_by_balance: false, trades_abroad: false, kept_by_operations: false, foreign_broker: false, tax_owner_user_id: null, counted_by: "balance",
};

// Decision Р-51 А: 16 % from January, the key rate less 1.5 from September.
const stated: AccountRates = {
  rates: [
    { from_on: "2026-01-01", kind: "fixed", percent: "16.000", by_hand: true },
    { from_on: "2026-09-01", kind: "key", percent: "-1.500", by_hand: true },
  ],
  current: { percent: "15.500", from_on: "2026-09-01", kind: "key", by_hand: true },
  key_rate: "17",
};

function answer(r: AccountRates) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(new Response(JSON.stringify(r), { status: 200, headers: { "Content-Type": "application/json" } })),
  );
}

function show(canEdit = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <RatePanel account={deposit} canEdit={canEdit} />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("RatePanel", () => {
  it("shows the rate in force, how it is struck, that it is by hand, and the ones before", async () => {
    answer(stated);
    show();
    expect(norm((await screen.findByTestId("account-rate-now")).textContent)).toBe("15,5 % годовых");
    const panel = norm(screen.getByTestId("account-rate").textContent);
    expect(panel).toContain("ключевая 17 % − 1,5 % · с 01.09.2026 · указана вручную");
    const rows = within(screen.getByTestId("account-rate-history")).getAllByRole("listitem").map((li) => norm(li.textContent));
    expect(rows).toEqual(["с 01.09.2026 — ключевая ставка ЦБ − 1,5 % · вручную", "с 01.01.2026 — 16 % годовых · вручную"]);
  });

  it("says what a goal grows at without a rate, and offers one only to an editor", async () => {
    answer({ rates: [], current: null, key_rate: "17" });
    show(false);
    expect(await screen.findByText(/Ставка не указана/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Указать ставку" })).toBeNull();
  });

  it("states a rate tied to the key rate, showing what it comes to", async () => {
    answer({ rates: [], current: null, key_rate: "17" });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Указать ставку" }));
    const dialog = await screen.findByTestId("account-rate-dialog");
    fireEvent.click(within(dialog).getByRole("combobox"));
    fireEvent.click(await screen.findByRole("option", { name: "Ключевая ставка ЦБ плюс или минус" }));
    fireEvent.change(within(dialog).getByLabelText("Плюс или минус к ключевой, %"), { target: { value: "−1,5" } });
    fireEvent.change(within(dialog).getByLabelText("Действует с"), { target: { value: "2026-09-01" } });
    expect(norm(dialog.textContent)).toContain("Сейчас ключевая 17 % — по счёту выйдет 15,5 % годовых.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(new URL(put.url).pathname).toBe("/api/v1/accounts/dep-1/rates");
    expect(await put.json()).toEqual({ from_on: "2026-09-01", kind: "key", percent: "-1.5" });
  });
});
