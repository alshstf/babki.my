import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { StructurePage } from "./index";

const asked = vi.hoisted(() => [] as URL[]);
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}
fetchMock.mockImplementation(async (input: Request) => {
  const url = new URL(input.url);
  const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
  if (url.pathname.endsWith("/structure")) {
    asked.push(url);
    return json({
      base_currency: "RUB", valuation: url.searchParams.get("valuation") ?? "liquid",
      assets_minor: 610_000_00, debts_minor: -30_000_00,
      by_class: [{ key: "shares", minor: 390_000_00 }, { key: "money", minor: 100_000_00 }, { key: "bonds", minor: 100_000_00 }, { key: "broker_cash", minor: 20_000_00 }],
      by_currency: [{ key: "RUB", minor: 520_000_00 }, { key: "USD", minor: 90_000_00 }],
      by_account: [{ key: "a-1", minor: 510_000_00 }, { key: "a-2", minor: 100_000_00 }],
      by_country: [{ key: "RU", minor: 400_000_00 }, { key: "", minor: 120_000_00 }, { key: "US", minor: 90_000_00 }],
      unpriced: 1, missing_rates: [],
    });
  }
  if (url.pathname.endsWith("/accounts")) {
    return json([{ id: "a-1", name: "Брокер" }, { id: "a-2", name: "Карта" }]);
  }
  return new Response("null", { status: 404 });
});

afterEach(() => {
  cleanup();
  asked.length = 0;
});

describe("StructurePage", () => {
  it("takes the worth apart four ways and asks again for the full valuation", async () => {
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <StructurePage />
      </QueryClientProvider>,
    );
    expect(await screen.findByTestId("structure-net")).toHaveTextContent(/580\s000/);
    const classes = within(screen.getByTestId("structure-class")).getAllByTestId("structure-slice");
    expect(classes[0]).toHaveTextContent(/Акции и расписки.*390\s000.*64 %/);
    expect(within(screen.getByTestId("structure-account")).getByText("Карта")).toBeTruthy();
    expect(within(screen.getByTestId("structure-country")).getByText(/Не бумаги/)).toBeTruthy();
    expect(screen.getByText(/Бумаг без цены не учтено: 1/)).toBeTruthy();

    fireEvent.keyDown(screen.getByRole("combobox", { name: "Оценка бумаг" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: /Полная/ }));
    await waitFor(() => expect(asked.some((u) => u.searchParams.get("valuation") === "full")).toBe(true));
  });
});
