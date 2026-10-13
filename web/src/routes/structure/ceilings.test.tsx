import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { CeilingsCard } from "./ceilings";
import type { Allocation, AllocationRow } from "@/api/allocation";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const row = (key: string, minor: number, share: string, target?: string, off?: number): AllocationRow => ({
  key, minor, share, target: target ?? null, gap: null, off_minor: off ?? null,
});

// Papers of 1 000 000: a quarter American at a ceiling of 20 %.
const allocation: Allocation = {
  base_currency: "RUB", mode: "topup", worth_minor: 1_200_000_00, everyday_minor: 0, unsplit_minor: 0,
  kinds: [], currencies: [],
  countries: [row("RU", 750_000_00, "75.00"), row("US", 250_000_00, "25.00", "20.00", 50_000_00)],
  sectors: [row("it", 350_000_00, "35.00"), row("government", 400_000_00, "40.00"), row("-", 250_000_00, "25.00")],
  papers_minor: 1_000_000_00, unpriced: 0, missing_rates: [],
};

function show(role: "owner" | "viewer") {
  fetchMock.mockImplementation((input: Request) => {
    const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
    const url = new URL(input.url);
    if (url.pathname.endsWith("/auth/me")) return json({ user: { id: "u", username: "a", display_name: "A" }, role, space: { id: "s", name: "S", base_currency: "RUB" } });
    return json(allocation);
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <CeilingsCard valuation="liquid" />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("CeilingsCard", () => {
  it("shows the papers against the ceilings and states them", async () => {
    show("owner");
    const countries = await screen.findByTestId("ceilings-countries");
    const rows = within(countries).getAllByTestId("ceiling-row");
    expect(rows[1]).toHaveTextContent(/Соединенные Штаты.*25 % · потолок 20 %.*Выше потолка на 50\s000 ₽/);
    expect(rows[0]).not.toHaveTextContent(/потолок/);
    fireEvent.click(within(screen.getByTestId("ceilings-sectors")).getByRole("button", { name: "Задать потолки" }));
    const dialog = await screen.findByTestId("ceilings-dialog");
    expect(within(dialog).queryByText("Отрасль неизвестна")).toBeNull();
    fireEvent.change(within(dialog).getByLabelText(/^IT/), { target: { value: "30" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(await put.json()).toEqual({ dimension: "sector", targets: [{ key: "it", percent: "30.00" }] });
  });

  it("gives a viewer nothing to change", async () => {
    show("viewer");
    await screen.findByTestId("ceilings-countries");
    expect(screen.queryByRole("button", { name: /потолки/ })).toBeNull();
  });
});
