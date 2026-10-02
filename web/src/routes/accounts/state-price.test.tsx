import type { ReactElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { PositionsTable } from "./positions-table";
import { StatePriceDialog } from "./state-price-dialog";
import type { Position } from "@/api/positions";

const sent = vi.hoisted(() => [] as { url: string; body: unknown }[]);
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});
fetchMock.mockImplementation(async (input: Request) => {
  sent.push({ url: input.url, body: await input.clone().json() });
  return new Response(null, { status: 204 });
});

afterEach(() => {
  cleanup();
  sent.length = 0;
});

function wrap(ui: ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

const fund = (overrides: Partial<Position>): Position =>
  ({
    instrument: {
      id: "fund-1", type: "etf", name: "Замороженный фонд", ticker: "FXUS", isin: "", figi: "", currency: "RUB", frozen: true,
    },
    quantity: "10",
    cost_minor: 100_000,
    realized_pnl_minor: 0,
    settled_minor: 0,
    total_minor: 0,
    income_minor: 0,
    income_by_currency: [],
    fees_minor: 0,
    currency: "RUB",
    market_value_minor: null,
    market_value_currency: null,
    price: null,
    price_on: null,
    unrealized_pnl_minor: null,
    has_undated_lots: false,
    has_undated_realizations: false,
    market_value_gap: "no_quote",
    ...overrides,
  }) as unknown as Position;

describe("a paper nobody quotes", () => {
  it("offers a price by hand, and says when the price is one", () => {
    const onStatePrice = vi.fn();
    const { unmount } = wrap(
      <PositionsTable positions={[fund({})]} cash={[]} mode="native" baseCurrency="RUB" onStatePrice={onStatePrice} />,
    );
    fireEvent.click(screen.getByTestId("position-state-price"));
    expect(onStatePrice).toHaveBeenCalledWith({ id: "fund-1", name: "Замороженный фонд", currency: "RUB", bond: false });
    unmount();

    wrap(
      <PositionsTable
        positions={[fund({ market_value_minor: 50_000, market_value_currency: "RUB", price: "50", price_on: "2026-09-01", market_value_gap: null, price_by_hand: true } as Partial<Position>)]}
        cash={[]}
        mode="native"
        baseCurrency="RUB"
        onStatePrice={onStatePrice}
      />,
    );
    expect(screen.getByTestId("position-price-by-hand").textContent).toContain("цена указана вручную");
    expect(screen.queryByTestId("position-state-price")).toBeNull();
  });

  it("offers no price for a kind of paper the program does not value", () => {
    wrap(
      <PositionsTable
        positions={[fund({ market_value_gap: "type_not_priced" } as Partial<Position>)]}
        cash={[]}
        mode="native"
        baseCurrency="RUB"
        onStatePrice={vi.fn()}
      />,
    );
    expect(screen.queryByTestId("position-state-price")).toBeNull();
  });

  it("states the price for the day given", async () => {
    wrap(<StatePriceDialog open onOpenChange={() => {}} paper={{ id: "fund-1", name: "Фонд", currency: "RUB", bond: false }} />);
    fireEvent.change(screen.getByLabelText("На дату"), { target: { value: "2026-09-01" } });
    fireEvent.change(screen.getByLabelText(/Цена за штуку/), { target: { value: "50,25" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].url).toMatch(/\/api\/v1\/instruments\/fund-1\/prices$/);
    expect(sent[0].body).toEqual({ on: "2026-09-01", price: "50.25" });
  });
});
