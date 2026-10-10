import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { RecurringPayments } from "./recurring";
import type { RecurringPayment } from "@/api/recurring";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const payment = (name: string, hidden: boolean): RecurringPayment => ({
  name, cadence: "monthly", amount_minor: -1_500_00, currency: "RUB", account_id: "acc", category_id: null,
  last_on: "2026-10-05", next_on: "2026-11-05", count: 4, overdue: false, hidden,
});

function answer(role: string, list: RecurringPayment[]) {
  fetchMock.mockImplementation((input: Request) => {
    const path = new URL(input.url).pathname;
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(body === null ? null : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
    if (path.endsWith("/recurring/hidden")) return json(null, 204);
    if (path.endsWith("/recurring")) return json(list);
    if (path.endsWith("/auth/me")) return json({ user: { id: "u", username: "a", display_name: "A" }, role, space: { id: "s", name: "S", base_currency: "RUB" } });
    return json([]);
  });
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <RecurringPayments />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("RecurringPayments: not regular after all", () => {
  it("hides a payment and lists the hidden apart, to be taken back", async () => {
    answer("owner", [payment("Аренда", false), payment("Пятёрочка", true)]);
    show();
    expect(await screen.findAllByTestId("recurring-row")).toHaveLength(1);
    fireEvent.click(await screen.findByRole("button", { name: "Не считать регулярным" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(await put.json()).toEqual({ name: "Аренда", incoming: false, currency: "RUB" });

    fireEvent.click(screen.getByRole("button", { name: "Скрытые: 1" }));
    fireEvent.click(await screen.findByRole("button", { name: "вернуть" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "DELETE")).toBe(true));
  });

  it("leaves a viewer only the list", async () => {
    answer("viewer", [payment("Аренда", false)]);
    show();
    await screen.findAllByTestId("recurring-row");
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByRole("button", { name: "Не считать регулярным" })).toBeNull();
  });
});
